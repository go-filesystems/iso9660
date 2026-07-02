// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"
)

// errReaderAt is an io.ReaderAt whose ReadAt always fails, standing in for a
// truncated/unreadable backing image so the on-disk read error branches (which
// a well-formed in-memory image never triggers) can be exercised deterministically.
type errReaderAt struct{}

func (errReaderAt) ReadAt([]byte, int64) (int, error) { return 0, io.ErrUnexpectedEOF }

// TestReadOnlyMutators asserts every mutating method returns ErrReadOnly:
// ISO 9660 is a read-only format.
func TestReadOnlyMutators(t *testing.T) {
	fs := &FS{}
	if err := fs.WriteFile("/x", nil, 0); !errors.Is(err, ErrReadOnly) {
		t.Errorf("WriteFile = %v, want ErrReadOnly", err)
	}
	if err := fs.MkDir("/x", 0); !errors.Is(err, ErrReadOnly) {
		t.Errorf("MkDir = %v, want ErrReadOnly", err)
	}
	if err := fs.DeleteFile("/x"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("DeleteFile = %v, want ErrReadOnly", err)
	}
	if err := fs.DeleteDir("/x"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("DeleteDir = %v, want ErrReadOnly", err)
	}
	if err := fs.Rename("/x", "/y"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Rename = %v, want ErrReadOnly", err)
	}
}

// TestOpenFile covers the file-backed entry point: a good image opens and reads
// back; a missing path and a non-ISO file both fail without panicking. Close on
// the file-backed handle releases the descriptor.
func TestOpenFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "ok.iso")
	if err := os.WriteFile(good, multiExtentISO, 0o644); err != nil {
		t.Fatal(err)
	}
	fs, err := OpenFile(good)
	if err != nil {
		t.Fatalf("OpenFile(good): %v", err)
	}
	if _, err := fs.ReadFile("/PLAIN.TXT"); err != nil {
		t.Errorf("ReadFile after OpenFile: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	if _, err := OpenFile(filepath.Join(dir, "nope.iso")); err == nil {
		t.Error("OpenFile(missing): want error, got nil")
	}

	bad := filepath.Join(dir, "bad.iso")
	if err := os.WriteFile(bad, bytes.Repeat([]byte{0}, 40*sectorSize), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(bad); err == nil {
		t.Error("OpenFile(non-ISO): want error, got nil")
	}
}

// TestCloseNoCloser verifies Close is a no-op (nil) when the backing reader is
// not an io.Closer (e.g. a bytes.Reader supplied to Open).
func TestCloseNoCloser(t *testing.T) {
	fs, err := Open(bytes.NewReader(multiExtentISO), int64(len(multiExtentISO)))
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Close(); err != nil {
		t.Errorf("Close (no closer) = %v, want nil", err)
	}
}

// ucs2be encodes s as the big-endian UCS-2 a Joliet directory record stores.
func ucs2be(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, r := range u {
		binary.BigEndian.PutUint16(b[2*i:], r)
	}
	return b
}

// jolietImage builds an in-memory image carrying both a base Primary tree and a
// Joliet Supplementary Volume Descriptor (UCS-2 long names). With no Rock Ridge
// present the reader must select the Joliet tree and decode its names. This
// covers the Joliet path (isJolietEscape, jolietName, chooseTree's Joliet
// branch, effectiveName's Joliet branch) without needing a mastering tool.
func jolietImage(t *testing.T) []byte {
	t.Helper()
	const (
		pvdLBA      = 16
		svdLBA      = 17
		termLBA     = 18
		baseRootLBA = 19
		jolRootLBA  = 20
		fileLBA     = 21
		totalBlocks = 22
	)
	img := make([]byte, totalBlocks*sectorSize)

	pvd := img[pvdLBA*sectorSize:]
	pvd[0] = vdTypePrimary
	copy(pvd[1:6], standardID)
	binary.LittleEndian.PutUint32(pvd[80:], totalBlocks)
	binary.LittleEndian.PutUint16(pvd[128:], sectorSize)
	copy(pvd[156:], buildDirRecord(baseRootLBA, sectorSize, flagDirectory, []byte{0x00}))

	svd := img[svdLBA*sectorSize:]
	svd[0] = vdTypeSupplement
	copy(svd[1:6], standardID)
	copy(svd[88:], jolietEscapes[2]) // "%/E" — UCS-2 level 3
	copy(svd[156:], buildDirRecord(jolRootLBA, sectorSize, flagDirectory, []byte{0x00}))

	term := img[termLBA*sectorSize:]
	term[0] = vdTypeTerminator
	copy(term[1:6], standardID)

	// Base Primary root extent (traversed only for the "." SUSP probe).
	base := img[baseRootLBA*sectorSize:]
	bp := 0
	putBase := func(rec []byte) { copy(base[bp:], rec); bp += len(rec) }
	putBase(buildDirRecord(baseRootLBA, sectorSize, flagDirectory, []byte{0x00}))
	putBase(buildDirRecord(baseRootLBA, sectorSize, flagDirectory, []byte{0x01}))

	// Joliet root extent: ".", ".." and one UCS-2 file with a ";1" version.
	jol := img[jolRootLBA*sectorSize:]
	jp := 0
	putJol := func(rec []byte) { copy(jol[jp:], rec); jp += len(rec) }
	putJol(buildDirRecord(jolRootLBA, sectorSize, flagDirectory, []byte{0x00}))
	putJol(buildDirRecord(jolRootLBA, sectorSize, flagDirectory, []byte{0x01}))
	body := []byte("joliet body\n")
	putJol(buildDirRecord(fileLBA, uint32(len(body)), 0x00, ucs2be("Hello World.txt;1")))

	copy(img[fileLBA*sectorSize:], body)
	return img
}

// TestJolietTree exercises the Joliet decode path end-to-end on an in-memory
// image (the tool-backed interop test is skipped when genisoimage is absent).
func TestJolietTree(t *testing.T) {
	fs, err := Open(bytes.NewReader(jolietImage(t)), -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !fs.joliet {
		t.Fatal("expected the Joliet tree to be selected")
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "Hello World.txt" {
		t.Fatalf("ListDir(/) = %v; want [Hello World.txt] (UCS-2, version stripped)", entries)
	}
	if got, err := fs.ReadFile("/Hello World.txt"); err != nil || !bytes.Equal(got, []byte("joliet body\n")) {
		t.Fatalf("ReadFile: got %q err %v", got, err)
	}
}

// TestJolietNameSpecials covers the "." / ".." single-byte special cases and the
// trailing-dot trim of jolietName, which the directory-listing path never
// reaches (it filters the specials before decoding names).
func TestJolietNameSpecials(t *testing.T) {
	if got := jolietName([]byte{0x00}); got != "." {
		t.Errorf("jolietName(0x00) = %q, want .", got)
	}
	if got := jolietName([]byte{0x01}); got != ".." {
		t.Errorf("jolietName(0x01) = %q, want ..", got)
	}
	if got := jolietName(ucs2be("NOEXT.")); got != "NOEXT" {
		t.Errorf("jolietName(NOEXT.) = %q, want NOEXT", got)
	}
}

// TestMergeMultiExtentOverflow: a multi-extent run whose extent sizes sum past
// 4 GiB must be rejected (the merged Size is a u32), not silently truncated.
func TestMergeMultiExtentOverflow(t *testing.T) {
	name := []byte("HUGE.BIN;1")
	in := []dirRecord{
		{Size: 0xFFFFFFFF, Flags: flagMultiExt, rawName: name, ExtentLBA: 19},
		{Size: 0x00000010, Flags: 0x00, rawName: name, ExtentLBA: 20},
	}
	if _, err := mergeMultiExtent(in); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("mergeMultiExtent overflow: err = %v, want ErrCorrupt", err)
	}
}

// TestReadFileReadError covers readFile's on-disk read-failure branch: a valid
// extent list whose backing store cannot be read yields a wrapped I/O error, not
// a panic or partial buffer.
func TestReadFileReadError(t *testing.T) {
	vol := &Volume{BlockSize: sectorSize}
	rec := dirRecord{Size: 10, extents: []extent{{lba: 19, size: 10}}}
	if _, err := readFile(errReaderAt{}, vol, rec, 1<<20); err == nil {
		t.Fatal("readFile with failing reader: want error, got nil")
	}
}

// TestReadDirRecordsReadError covers readDirRecords' on-disk read-failure branch.
func TestReadDirRecordsReadError(t *testing.T) {
	vol := &Volume{BlockSize: sectorSize}
	rec := dirRecord{Flags: flagDirectory, Size: sectorSize, ExtentLBA: 18}
	if _, err := readDirRecords(errReaderAt{}, vol, rec, 1<<20); err == nil {
		t.Fatal("readDirRecords with failing reader: want error, got nil")
	}
}

// TestReadFileNotRegular / non-dir guards.
func TestTypeGuards(t *testing.T) {
	vol := &Volume{BlockSize: sectorSize}
	if _, err := readFile(bytes.NewReader(make([]byte, sectorSize)), vol,
		dirRecord{Flags: flagDirectory}, 1<<20); !errors.Is(err, ErrNotRegular) {
		t.Errorf("readFile(dir) = %v, want ErrNotRegular", err)
	}
	if _, err := readDirRecords(bytes.NewReader(make([]byte, sectorSize)), vol,
		dirRecord{Flags: 0x00}, 1<<20); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("readDirRecords(file) = %v, want ErrNotDirectory", err)
	}
}

// TestParseDirRecordErrors covers the malformed-length branches of parseDirRecord.
func TestParseDirRecordErrors(t *testing.T) {
	// Empty / zero-length slot: no error, zero bytes consumed.
	if rec, n, err := parseDirRecord(nil); err != nil || n != 0 || rec.Length != 0 {
		t.Errorf("parseDirRecord(nil) = %+v,%d,%v", rec, n, err)
	}
	// Length below the 33-byte minimum.
	if _, _, err := parseDirRecord([]byte{10, 0, 0}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("parseDirRecord(short) = %v, want ErrCorrupt", err)
	}
	// Length exceeding the buffer.
	if _, _, err := parseDirRecord([]byte{40, 0, 0}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("parseDirRecord(overlong) = %v, want ErrCorrupt", err)
	}
	// Name length overflowing the record.
	buf := make([]byte, 34)
	buf[0] = 34   // record length
	buf[32] = 100 // name length far beyond the record
	if _, _, err := parseDirRecord(buf); !errors.Is(err, ErrCorrupt) {
		t.Errorf("parseDirRecord(nameoverflow) = %v, want ErrCorrupt", err)
	}
}

// TestReadDirRecordsMalformedEntry covers readDirRecords propagating a
// parseDirRecord error when a directory extent contains a malformed record
// (a non-zero length below the 33-byte minimum).
func TestReadDirRecordsMalformedEntry(t *testing.T) {
	vol := &Volume{BlockSize: sectorSize}
	img := make([]byte, sectorSize)
	img[0] = 10 // record length below the minimum, non-zero => corrupt
	rec := dirRecord{Flags: flagDirectory, Size: sectorSize, ExtentLBA: 0}
	if _, err := readDirRecords(bytes.NewReader(img), vol, rec, int64(len(img))); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("readDirRecords(malformed) = %v, want ErrCorrupt", err)
	}
}

// TestMergeMultiExtentNameMismatch covers the run-terminating branch where a
// record carrying the multi-extent flag is followed by a record with a
// different identifier: the run has no final extent and is rejected as corrupt.
func TestMergeMultiExtentNameMismatch(t *testing.T) {
	in := []dirRecord{
		{Size: 100, Flags: flagMultiExt, rawName: []byte("A.BIN;1"), ExtentLBA: 19},
		{Size: 100, Flags: 0x00, rawName: []byte("B.BIN;1"), ExtentLBA: 20},
	}
	if _, err := mergeMultiExtent(in); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("mergeMultiExtent(name mismatch) = %v, want ErrCorrupt", err)
	}
}

// TestIsJolietEscape covers both arms of the escape-sequence check.
func TestIsJolietEscape(t *testing.T) {
	if !isJolietEscape(jolietEscapes[0]) {
		t.Error("isJolietEscape(%/@) = false, want true")
	}
	if isJolietEscape([]byte("not-joliet")) {
		t.Error("isJolietEscape(garbage) = true, want false")
	}
	if isJolietEscape([]byte{}) {
		t.Error("isJolietEscape(empty) = true, want false")
	}
}

// TestBlockSizeDefaulted covers readVolume defaulting a zero logical block size
// to the 2048-byte sector, and TestReadVolumeBadRoot covers a malformed PVD root
// directory record.
func TestReadVolumeQuirks(t *testing.T) {
	// Zero logical block size in the PVD must default to sectorSize.
	img := minimalImage(8, 0x00, "")
	binary.LittleEndian.PutUint16(img[16*sectorSize+128:], 0)
	vol, err := readVolume(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("readVolume(blocksize 0): %v", err)
	}
	if vol.BlockSize != sectorSize {
		t.Errorf("BlockSize = %d, want %d (defaulted)", vol.BlockSize, sectorSize)
	}

	// A PVD whose root directory record is malformed must surface an error.
	bad := minimalImage(8, 0x00, "")
	bad[16*sectorSize+156] = 10 // root record length below the 33-byte minimum
	if _, err := readVolume(bytes.NewReader(bad)); err == nil {
		t.Error("readVolume(bad root record): want error, got nil")
	}
}

// TestResolveThroughNonDir covers path resolution failing when an intermediate
// component is not a directory, and ListDir surfacing a resolve error.
func TestResolveThroughNonDir(t *testing.T) {
	fs, err := Open(bytes.NewReader(multiExtentISO), int64(len(multiExtentISO)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadFile("/PLAIN.TXT/inner"); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("ReadFile through non-dir = %v, want ErrNotDirectory", err)
	}
	if _, err := fs.ListDir("/does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ListDir(missing) = %v, want ErrNotFound", err)
	}
}

// appendSUA splices an arbitrary System Use Area onto a directory record built
// by buildDirRecord (which emits none) and fixes the record-length byte.
func appendSUA(rec, sua []byte) []byte {
	out := append(append([]byte(nil), rec...), sua...)
	out[0] = byte(len(out))
	return out
}

func spEntry() []byte { return []byte{'S', 'P', 7, 1, 0xBE, 0xEF, 0} }
func nmEntry(name string) []byte {
	return append([]byte{'N', 'M', byte(5 + len(name)), 1, 0x00}, name...)
}
func pxEntry(mode uint32) []byte {
	e := []byte{'P', 'X', 8, 1, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(e[4:], mode)
	return e
}
func slEntry(target string) []byte {
	// One component: flags=0 (complete), clen, content.
	return append([]byte{'S', 'L', byte(7 + len(target)), 1, 0x00, 0x00, byte(len(target))}, target...)
}

// rrImage builds an in-memory Rock Ridge image: the root "." carries SP (the
// SUSP/Rock Ridge signal), a plain file carries NM (real long name) + PX (POSIX
// mode), and a symlink carries NM + SL (target). This drives the Rock Ridge
// decode path — detectSUSPSkip, parseRockRidge's NM/PX/SL arms, effectiveName's
// Rock Ridge branch, Stat's POSIX-mode branch and ReadLink — without a mastering
// tool.
func rrImage(t *testing.T) []byte {
	t.Helper()
	const (
		pvdLBA      = 16
		termLBA     = 17
		rootLBA     = 18
		fileLBA     = 19
		totalBlocks = 20
	)
	img := make([]byte, totalBlocks*sectorSize)

	pvd := img[pvdLBA*sectorSize:]
	pvd[0] = vdTypePrimary
	copy(pvd[1:6], standardID)
	binary.LittleEndian.PutUint32(pvd[80:], totalBlocks)
	binary.LittleEndian.PutUint16(pvd[128:], sectorSize)
	copy(pvd[156:], buildDirRecord(rootLBA, sectorSize, flagDirectory, []byte{0x00}))

	term := img[termLBA*sectorSize:]
	term[0] = vdTypeTerminator
	copy(term[1:6], standardID)

	root := img[rootLBA*sectorSize:]
	pos := 0
	put := func(rec []byte) { copy(root[pos:], rec); pos += len(rec) }
	// "." carries SP so the reader recognises Rock Ridge.
	put(appendSUA(buildDirRecord(rootLBA, sectorSize, flagDirectory, []byte{0x00}), spEntry()))
	put(buildDirRecord(rootLBA, sectorSize, flagDirectory, []byte{0x01}))

	body := []byte("rr body\n")
	// Plain file: base name PLAIN.TXT, Rock Ridge name "Real Name.txt", mode 0644.
	plain := appendSUA(
		buildDirRecord(fileLBA, uint32(len(body)), 0x00, []byte("PLAIN.TXT;1")),
		append(nmEntry("Real Name.txt"), pxEntry(0o100644)...),
	)
	put(plain)
	// Symlink: base name LINK, Rock Ridge name "link", SL target "Real Name.txt".
	link := appendSUA(
		buildDirRecord(fileLBA, 0, 0x00, []byte("LINK.;1")),
		append(nmEntry("link"), slEntry("Real Name.txt")...),
	)
	put(link)

	copy(img[fileLBA*sectorSize:], body)
	return img
}

// TestRockRidgeTree exercises the Rock Ridge decode path end-to-end on an
// in-memory image (the tool-backed interop tests are skipped without genisoimage).
func TestRockRidgeTree(t *testing.T) {
	fs, err := Open(bytes.NewReader(rrImage(t)), -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if fs.joliet {
		t.Fatal("Rock Ridge tree should be selected, not Joliet")
	}
	// effectiveName resolves the Rock Ridge NM name.
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names["Real Name.txt"] || !names["link"] {
		t.Fatalf("ListDir names = %v; want Real Name.txt and link", names)
	}
	// Stat surfaces the Rock Ridge POSIX mode.
	st, err := fs.Stat("/Real Name.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Mode()&0o777 != 0o644 {
		t.Errorf("Stat mode = %o, want 0644 perms", st.Mode())
	}
	// ReadFile through the Rock Ridge name works.
	if got, err := fs.ReadFile("/Real Name.txt"); err != nil || !bytes.Equal(got, []byte("rr body\n")) {
		t.Fatalf("ReadFile = %q err %v", got, err)
	}
	// ReadLink returns the SL target.
	if tgt, err := fs.ReadLink("/link"); err != nil || tgt != "Real Name.txt" {
		t.Fatalf("ReadLink = %q err %v; want Real Name.txt", tgt, err)
	}
	// ReadLink on a non-symlink reports ErrNotSymlink.
	if _, err := fs.ReadLink("/Real Name.txt"); !errors.Is(err, ErrNotSymlink) {
		t.Errorf("ReadLink(non-symlink) = %v, want ErrNotSymlink", err)
	}
}

// ceEntryBytes builds a SUSP CE (continuation) entry pointing at block/offset
// for length bytes. Only the little-endian halves of the both-endian fields are
// filled; the reader consumes those.
func ceEntryBytes(block, offset, length uint32) []byte {
	e := make([]byte, 28)
	e[0], e[1], e[2], e[3] = 'C', 'E', 28, 1
	binary.LittleEndian.PutUint32(e[4:], block)   // d[0:]  block
	binary.LittleEndian.PutUint32(e[12:], offset) // d[8:]  offset
	binary.LittleEndian.PutUint32(e[20:], length) // d[16:] length
	return e
}

// TestRockRidgeCEContinuation covers collectSUSP following a CE entry into a
// separate continuation area to recover an NM name that does not fit in the
// directory record's own System Use Area.
func TestRockRidgeCEContinuation(t *testing.T) {
	const (
		pvdLBA      = 16
		termLBA     = 17
		rootLBA     = 18
		fileLBA     = 19
		ceLBA       = 20
		totalBlocks = 21
	)
	img := make([]byte, totalBlocks*sectorSize)

	pvd := img[pvdLBA*sectorSize:]
	pvd[0] = vdTypePrimary
	copy(pvd[1:6], standardID)
	binary.LittleEndian.PutUint32(pvd[80:], totalBlocks)
	binary.LittleEndian.PutUint16(pvd[128:], sectorSize)
	copy(pvd[156:], buildDirRecord(rootLBA, sectorSize, flagDirectory, []byte{0x00}))

	term := img[termLBA*sectorSize:]
	term[0] = vdTypeTerminator
	copy(term[1:6], standardID)

	root := img[rootLBA*sectorSize:]
	pos := 0
	put := func(rec []byte) { copy(root[pos:], rec); pos += len(rec) }
	put(appendSUA(buildDirRecord(rootLBA, sectorSize, flagDirectory, []byte{0x00}), spEntry()))
	put(buildDirRecord(rootLBA, sectorSize, flagDirectory, []byte{0x01}))

	// The file's own SUA holds only a CE pointing at the continuation block; the
	// NM entry lives there.
	cont := nmEntry("Continued Long Name.txt")
	copy(img[ceLBA*sectorSize:], cont)
	file := appendSUA(
		buildDirRecord(fileLBA, uint32(len("body")), 0x00, []byte("F.TXT;1")),
		ceEntryBytes(ceLBA, 0, uint32(len(cont))),
	)
	put(file)
	copy(img[fileLBA*sectorSize:], []byte("body"))

	fs, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "Continued Long Name.txt" {
		t.Fatalf("ListDir = %v; want the CE-continued name", entries)
	}
}

// TestCollectSUSPUnreadableCE covers collectSUSP's defensive break when a CE
// entry points at a block outside the image (the continuation read fails): the
// original System Use Area is returned unextended rather than propagating an error.
func TestCollectSUSPUnreadableCE(t *testing.T) {
	fs := &FS{rs: bytes.NewReader(make([]byte, 4*sectorSize)), vol: &Volume{BlockSize: sectorSize}}
	ce := ceEntryBytes(9999, 0, 10) // block 9999 is far beyond the 4-sector reader
	if got := fs.collectSUSP(ce); !bytes.Equal(got, ce) {
		t.Errorf("collectSUSP(unreadable CE) = %d bytes, want the %d-byte SUA unchanged", len(got), len(ce))
	}
}

// TestParseRockRidgeComponents covers the SL "." / ".." / "/" component flags
// and the NM CONTINUE flag directly, plus a truncated trailing entry that must
// break the scan rather than read out of bounds.
func TestParseRockRidgeComponents(t *testing.T) {
	// SL with cur(.), par(..), root(/) and a normal component.
	sl := []byte{'S', 'L', 0, 1, 0x00}
	comp := func(flags byte, s string) []byte {
		return append([]byte{flags, byte(len(s))}, s...)
	}
	body := comp(slCompRoot, "")      // "/"
	body = append(body, comp(slCompCur, "")...)          // "."
	body = append(body, comp(slCompPar, "")...)          // ".."
	body = append(body, comp(0x00, "etc")...)            // normal
	sl = append(sl, body...)
	sl[2] = byte(len(sl))
	rr := parseRockRidge(sl)
	if !rr.isSymlink {
		t.Fatal("parseRockRidge: expected symlink")
	}

	// NM with the CONTINUE flag set (exercises the continuation arm).
	nm := []byte{'N', 'M', 0, 1, nmContinue}
	nm = append(nm, []byte("part")...)
	nm[2] = byte(len(nm))
	if got := parseRockRidge(nm); !got.hasName || got.name != "part" {
		t.Errorf("parseRockRidge(NM continue) name = %q hasName=%v", got.name, got.hasName)
	}

	// A trailing entry claiming more length than remains must break cleanly.
	trunc := []byte{'N', 'M', 40, 1, 0x00, 'x'} // length 40 but only 6 bytes present
	if got := parseRockRidge(trunc); got.hasName {
		t.Errorf("parseRockRidge(truncated) hasName=%v, want false (broke on bad length)", got.hasName)
	}

	// An SL component whose declared length overruns the entry must break the
	// component scan rather than read out of bounds.
	badComp := []byte{'S', 'L', 8, 1, 0x00, 0x00, 200, 'x'} // comp clen 200, 1 byte present
	if got := parseRockRidge(badComp); got.symlink != "" {
		t.Errorf("parseRockRidge(bad SL comp) symlink=%q, want empty", got.symlink)
	}

	// detectSUSPSkip must skip past a non-SP entry to find a following SP.
	sua := append([]byte{'R', 'R', 5, 1, 0x00}, spEntry()...)
	if skip, found := detectSUSPSkip(sua); !found || skip != 0 {
		t.Errorf("detectSUSPSkip(RR then SP) = %d,%v; want 0,true", skip, found)
	}
	// Same defensive break in detectSUSPSkip and ceEntry.
	if _, found := detectSUSPSkip([]byte{'S', 'P', 40, 1}); found {
		t.Error("detectSUSPSkip(truncated) found=true, want false")
	}
	if _, _, _, found := ceEntry([]byte{'C', 'E', 40, 1}); found {
		t.Error("ceEntry(truncated) found=true, want false")
	}
}

// TestSymlinkLoop covers resolveFrom's hop-limit guard: a Rock Ridge symlink
// whose target is itself must terminate with ErrTooManyLinks, never loop forever.
func TestSymlinkLoop(t *testing.T) {
	const (
		pvdLBA      = 16
		termLBA     = 17
		rootLBA     = 18
		totalBlocks = 19
	)
	img := make([]byte, totalBlocks*sectorSize)
	pvd := img[pvdLBA*sectorSize:]
	pvd[0] = vdTypePrimary
	copy(pvd[1:6], standardID)
	binary.LittleEndian.PutUint32(pvd[80:], totalBlocks)
	binary.LittleEndian.PutUint16(pvd[128:], sectorSize)
	copy(pvd[156:], buildDirRecord(rootLBA, sectorSize, flagDirectory, []byte{0x00}))
	term := img[termLBA*sectorSize:]
	term[0] = vdTypeTerminator
	copy(term[1:6], standardID)

	root := img[rootLBA*sectorSize:]
	pos := 0
	put := func(rec []byte) { copy(root[pos:], rec); pos += len(rec) }
	put(appendSUA(buildDirRecord(rootLBA, sectorSize, flagDirectory, []byte{0x00}), spEntry()))
	put(buildDirRecord(rootLBA, sectorSize, flagDirectory, []byte{0x01}))
	// "loop" is a symlink whose target is "loop": self-referential cycle.
	put(appendSUA(
		buildDirRecord(rootLBA, 0, 0x00, []byte("LOOP.;1")),
		append(nmEntry("loop"), slEntry("loop")...),
	))

	fs, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := fs.ReadFile("/loop"); !errors.Is(err, ErrTooManyLinks) {
		t.Fatalf("ReadFile(/loop) = %v, want ErrTooManyLinks", err)
	}
}

// TestReadVolumeErrors covers readVolume's rejection branches.
func TestReadVolumeErrors(t *testing.T) {
	// Unreadable backing store.
	if _, err := readVolume(errReaderAt{}); err == nil {
		t.Error("readVolume(failing reader): want error, got nil")
	}
	// A descriptor lacking the CD001 standard identifier.
	bad := make([]byte, 20*sectorSize)
	if _, err := readVolume(bytes.NewReader(bad)); !errors.Is(err, ErrBadDescriptor) {
		t.Errorf("readVolume(no CD001) = %v, want ErrBadDescriptor", err)
	}
	// A terminator that appears before any Primary Volume Descriptor.
	term := make([]byte, 20*sectorSize)
	t0 := term[systemAreaSectors*sectorSize:]
	t0[0] = vdTypeTerminator
	copy(t0[1:6], standardID)
	if _, err := readVolume(bytes.NewReader(term)); !errors.Is(err, ErrBadDescriptor) {
		t.Errorf("readVolume(terminator-first) = %v, want ErrBadDescriptor", err)
	}
}
