// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import (
	"bytes"
	"sort"
	"testing"
)

// memImage is an in-memory io.WriterAt / io.ReaderAt sink for round-trip tests:
// the Builder writes into it and Open reads back out of it.
type memImage struct{ b []byte }

func (m *memImage) WriteAt(p []byte, off int64) (int, error) {
	end := int(off) + len(p)
	if end > len(m.b) {
		m.b = append(m.b, make([]byte, end-len(m.b))...)
	}
	copy(m.b[off:], p)
	return len(p), nil
}

func (m *memImage) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.b)) {
		return 0, errEOF
	}
	n := copy(p, m.b[off:])
	if n < len(p) {
		return n, errEOF
	}
	return n, nil
}

// errEOF mirrors io.EOF without importing io into the assertions.
var errEOF = bytesEOF{}

type bytesEOF struct{}

func (bytesEOF) Error() string { return "EOF" }

// buildImage masters a small tree and returns an opened FS reading it back.
func buildImage(t *testing.T, b *Builder) (*FS, *memImage) {
	t.Helper()
	img := &memImage{}
	if err := b.WriteTo(img); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	fs, err := Open(img, int64(len(img.b)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return fs, img
}

func listNames(t *testing.T, fs *FS, dir string) []string {
	t.Helper()
	entries, err := fs.ListDir(dir)
	if err != nil {
		t.Fatalf("ListDir(%s): %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// TestCreate_RoundTrip masters a tree with nested directories and several
// files, then reads it back through this package's own reader and asserts the
// names, sizes, byte contents and tree structure all match what was written.
func TestCreate_RoundTrip(t *testing.T) {
	readme := []byte("read-write optical filesystem\n")
	data := pattern(5000) // > 2 sectors: exercises a multi-sector file extent
	nested := []byte("nested entry\n")
	empty := []byte{}

	b := NewBuilder("TESTVOL")
	if err := b.AddFile("/README.TXT", readme); err != nil {
		t.Fatal(err)
	}
	if err := b.AddFile("/DATA.BIN", data); err != nil {
		t.Fatal(err)
	}
	if err := b.AddFile("/SUB/NESTED.TXT", nested); err != nil {
		t.Fatal(err)
	}
	if err := b.AddFile("/SUB/DEEP/LEAF.DAT", nested); err != nil {
		t.Fatal(err)
	}
	if err := b.AddFile("/EMPTY.DAT", empty); err != nil {
		t.Fatal(err)
	}
	if err := b.AddDir("/ONLYDIR"); err != nil {
		t.Fatal(err)
	}

	fs, _ := buildImage(t, b)
	defer fs.Close()

	// Volume identifier survives.
	if got := fs.Volume().VolumeID; got != "TESTVOL" {
		t.Errorf("VolumeID = %q, want %q", got, "TESTVOL")
	}

	// File contents and sizes round-trip exactly.
	want := map[string][]byte{
		"/README.TXT":        readme,
		"/DATA.BIN":          data,
		"/SUB/NESTED.TXT":    nested,
		"/SUB/DEEP/LEAF.DAT": nested,
		"/EMPTY.DAT":         empty,
	}
	for path, content := range want {
		got, err := fs.ReadFile(path)
		if err != nil {
			t.Errorf("ReadFile(%s): %v", path, err)
			continue
		}
		if !bytes.Equal(got, content) {
			t.Errorf("ReadFile(%s): %d bytes, content mismatch (want %d)", path, len(got), len(content))
		}
		st, err := fs.Stat(path)
		if err != nil {
			t.Errorf("Stat(%s): %v", path, err)
			continue
		}
		if st.Size() != uint64(len(content)) {
			t.Errorf("Stat(%s) size = %d, want %d", path, st.Size(), len(content))
		}
	}

	// Tree structure: root listing and nested listings.
	if got, want := listNames(t, fs, "/"), []string{"DATA.BIN", "EMPTY.DAT", "ONLYDIR", "README.TXT", "SUB"}; !equalStrings(got, want) {
		t.Errorf("ListDir(/) = %v, want %v", got, want)
	}
	if got, want := listNames(t, fs, "/SUB"), []string{"DEEP", "NESTED.TXT"}; !equalStrings(got, want) {
		t.Errorf("ListDir(/SUB) = %v, want %v", got, want)
	}
	if got, want := listNames(t, fs, "/SUB/DEEP"), []string{"LEAF.DAT"}; !equalStrings(got, want) {
		t.Errorf("ListDir(/SUB/DEEP) = %v, want %v", got, want)
	}
	if got := listNames(t, fs, "/ONLYDIR"); len(got) != 0 {
		t.Errorf("ListDir(/ONLYDIR) = %v, want empty", got)
	}

	// Directory Stat reports a directory mode.
	if st, err := fs.Stat("/SUB"); err != nil {
		t.Errorf("Stat(/SUB): %v", err)
	} else if st.Mode()&0xF000 != sIFDIR {
		t.Errorf("Stat(/SUB) mode = 0x%04x, want S_IFDIR bits", st.Mode())
	}
}

// TestCreate_NameMapping checks the interchange-level-1 identifier mapping:
// mixed-case and lowercase names become uppercase 8.3 ";1" identifiers, so the
// reader (which strips the version and matches case-insensitively) finds them.
func TestCreate_NameMapping(t *testing.T) {
	content := []byte("x")
	b := NewBuilder("vol")
	if err := b.AddFile("/readme.txt", content); err != nil {
		t.Fatal(err)
	}
	if err := b.AddFile("/MixedCase.Dat", content); err != nil {
		t.Fatal(err)
	}
	fs, _ := buildImage(t, b)
	defer fs.Close()

	// On-disk names are uppercased; the reader strips ";1".
	if got := listNames(t, fs, "/"); !equalStrings(got, []string{"MIXEDCAS.DAT", "README.TXT"}) {
		t.Errorf("ListDir(/) = %v, want [MIXEDCAS.DAT README.TXT]", got)
	}
	// Case-insensitive lookup reaches the file regardless of caller case.
	if got, err := fs.ReadFile("/readme.txt"); err != nil || !bytes.Equal(got, content) {
		t.Errorf("ReadFile(/readme.txt): err=%v equal=%v", err, bytes.Equal(got, content))
	}
}

// TestCreate_SectorLayout asserts standard-conformance facts about the raw
// bytes: the 16-sector system area is zeroed, the PVD and terminator carry the
// "CD001" standard identifier with the right descriptor types, and the logical
// block size is 2048.
func TestCreate_SectorLayout(t *testing.T) {
	b := NewBuilder("LAYOUT")
	if err := b.AddFile("/A.TXT", []byte("a")); err != nil {
		t.Fatal(err)
	}
	img := &memImage{}
	if err := b.WriteTo(img); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	raw := img.b

	if len(raw)%sectorSize != 0 {
		t.Errorf("image length %d is not a multiple of %d", len(raw), sectorSize)
	}
	// System area: first 16 sectors all zero.
	for i := 0; i < systemAreaSectors*sectorSize; i++ {
		if raw[i] != 0 {
			t.Fatalf("system area byte %d = 0x%02x, want 0", i, raw[i])
		}
	}
	// PVD at sector 16.
	pvd := raw[16*sectorSize : 17*sectorSize]
	if pvd[0] != vdTypePrimary {
		t.Errorf("PVD type = %d, want %d", pvd[0], vdTypePrimary)
	}
	if !bytes.Equal(pvd[1:6], standardID) {
		t.Errorf("PVD standard ID = %q, want CD001", pvd[1:6])
	}
	if bs := le16(pvd[128:]); bs != sectorSize {
		t.Errorf("logical block size = %d, want %d", bs, sectorSize)
	}
	// Both-endian volume space size must agree (LE at 80, BE at 84).
	if le, be := le32(pvd[80:]), beUint32(pvd[84:]); le != be {
		t.Errorf("volume space size both-endian mismatch: LE %d BE %d", le, be)
	}
	// Terminator at sector 17.
	term := raw[17*sectorSize : 18*sectorSize]
	if term[0] != vdTypeTerminator {
		t.Errorf("terminator type = %d, want %d", term[0], vdTypeTerminator)
	}
	if !bytes.Equal(term[1:6], standardID) {
		t.Errorf("terminator standard ID = %q, want CD001", term[1:6])
	}
}

// TestCreate_DisambiguatesNames ensures two caller names that map to the same
// level-1 identifier are made unique, so both files remain readable.
func TestCreate_DisambiguatesNames(t *testing.T) {
	b := NewBuilder("vol")
	// Both sanitize to the same 8.3 base; the builder must disambiguate.
	if err := b.AddFile("/longname-one.txt", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := b.AddFile("/longname-two.txt", []byte("two")); err != nil {
		t.Fatal(err)
	}
	fs, _ := buildImage(t, b)
	defer fs.Close()

	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("ListDir(/) returned %d entries, want 2: %v", len(entries), listNames(t, fs, "/"))
	}
	if entries[0].Name() == entries[1].Name() {
		t.Errorf("identifier collision not resolved: both %q", entries[0].Name())
	}
	// Both files are readable with distinct contents.
	seen := map[string]bool{}
	for _, e := range entries {
		data, err := fs.ReadFile("/" + e.Name())
		if err != nil {
			t.Errorf("ReadFile(/%s): %v", e.Name(), err)
		}
		seen[string(data)] = true
	}
	if !seen["one"] || !seen["two"] {
		t.Errorf("not both files round-tripped: %v", seen)
	}
}

// TestCreate_MultiSectorDir builds a directory with enough children that its
// record area spills past one 2048-byte sector, exercising the rule that a
// directory record never spans a sector boundary. Every file must still read
// back, and the PVD volume space size must match the written image length.
func TestCreate_MultiSectorDir(t *testing.T) {
	const n = 120 // each ~40-byte record; ~4.8 KiB > 2 sectors
	b := NewBuilder("BIGDIR")
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := nameForIndex(i)
		names = append(names, name)
		if err := b.AddFile("/D/"+name, []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	fs, img := buildImage(t, b)
	defer fs.Close()

	entries, err := fs.ListDir("/D")
	if err != nil {
		t.Fatalf("ListDir(/D): %v", err)
	}
	if len(entries) != n {
		t.Fatalf("ListDir(/D) returned %d entries, want %d", len(entries), n)
	}
	for i, name := range names {
		got, err := fs.ReadFile("/D/" + name)
		if err != nil || len(got) != 1 || got[0] != byte(i) {
			t.Errorf("ReadFile(/D/%s): err=%v got=%v", name, err, got)
		}
	}

	// PVD volume space size must equal the image length in sectors.
	pvd := img.b[16*sectorSize : 17*sectorSize]
	if space := le32(pvd[80:]); int(space)*sectorSize != len(img.b) {
		t.Errorf("volume space size = %d sectors (%d bytes), image is %d bytes",
			space, int(space)*sectorSize, len(img.b))
	}
}

// nameForIndex produces a distinct uppercase 8.3 base name for index i.
func nameForIndex(i int) string {
	const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	return "F" + string(digits[i/36%36]) + string(digits[i%36]) + ".BIN"
}

func beUint32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
