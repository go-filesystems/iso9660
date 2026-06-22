// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/go-volumes/safeio"
)

// THREAT MODEL: an untrusted ISO 9660 image must NEVER panic the host,
// OOB-read, integer-overflow into a bad alloc/slice, loop forever, or OOM.
// The tests below assert that hostile on-disk length fields turn into graceful
// errors (ErrCorrupt / safeio sentinels) rather than huge allocations or
// panics.

// minimalImage builds a tiny, structurally valid ISO 9660 image with one root
// directory holding a single regular file. fileSize/fileExtentSize is the Size
// field stamped into that file's directory record; fileFlags its file flags;
// when slTarget is non-empty a Rock Ridge SL System Use Area is attached so the
// file decodes as a symlink with that raw target. The returned image is small
// (a handful of sectors) so a forged 4 GiB Size cannot be backed by real data —
// exactly the "huge length in a tiny image" attack.
func minimalImage(fileSize uint32, fileFlags byte, slTarget string) []byte {
	const (
		blockSize   = 2048
		pvdLBA      = 16
		termLBA     = 17
		rootLBA     = 18
		fileLBA     = 19
		totalBlocks = 20
	)
	img := make([]byte, totalBlocks*blockSize)

	// Primary Volume Descriptor.
	pvd := img[pvdLBA*blockSize:]
	pvd[0] = vdTypePrimary
	copy(pvd[1:6], standardID)
	binary.LittleEndian.PutUint32(pvd[80:], totalBlocks)
	binary.LittleEndian.PutUint16(pvd[128:], blockSize)
	copy(pvd[156:], buildDirRecord(rootLBA, blockSize, flagDirectory, []byte{0x00}))

	// Volume-descriptor set terminator.
	term := img[termLBA*blockSize:]
	term[0] = vdTypeTerminator
	copy(term[1:6], standardID)

	// Root directory extent: ".", ".." and the target file.
	root := img[rootLBA*blockSize:]
	pos := 0
	put := func(rec []byte) {
		copy(root[pos:], rec)
		pos += len(rec)
	}
	put(buildDirRecord(rootLBA, blockSize, flagDirectory, []byte{0x00}))
	put(buildDirRecord(rootLBA, blockSize, flagDirectory, []byte{0x01}))

	name := []byte("F.BIN;1")
	rec := buildDirRecord(fileLBA, fileSize, fileFlags, name)
	if slTarget != "" {
		rec = appendSL(rec, slTarget)
	}
	put(rec)
	return img
}

// appendSL grows a directory record's length to carry a Rock Ridge SL (symlink)
// System Use Area whose single component is the raw, possibly hostile, target
// string. The base record produced by buildDirRecord has no SUA, so we splice
// one in after the (padded) name and bump the record length byte.
func appendSL(rec []byte, target string) []byte {
	// SL entry: "SL", LEN, version=1, flags=0, then one component:
	// compFlags=0, compLen, content. (No leading "/" handling here; the raw
	// target bytes are placed verbatim, which is what we want to fuzz.)
	comp := append([]byte{0x00, byte(len(target))}, []byte(target)...)
	sl := append([]byte{'S', 'L', byte(5 + len(comp)), 1, 0x00}, comp...)
	out := append(append([]byte(nil), rec...), sl...)
	out[0] = byte(len(out)) // record length must cover the SUA
	return out
}

// assertGraceful opens img and exercises the read paths, requiring that nothing
// panics and that any error is a graceful one. The recover guard converts an
// accidental panic into a test failure (the whole point of the hardening).
func assertGraceful(t *testing.T, img []byte, size int64) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on untrusted image: %v", r)
		}
	}()
	fs, err := Open(bytes.NewReader(img), size)
	if err != nil {
		return // a rejected image is a graceful outcome
	}
	// Every traversal/read entry point must survive a hostile image.
	_, _ = fs.ListDir("/")
	_, _ = fs.ReadFile("/F.BIN")
	_, _ = fs.Stat("/F.BIN")
	_, _ = fs.ReadLink("/F.BIN")
}

// TestSecurity_HugeFileSize: a file record whose Size is ~4 GiB in a tiny image
// must yield a bounded ErrCorrupt (wrapping safeio.ErrTooLarge), never a 4 GiB
// allocation or a short-read panic.
func TestSecurity_HugeFileSize(t *testing.T) {
	img := minimalImage(0xFFFFFFFF, 0x00, "")
	fs, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err = fs.ReadFile("/F.BIN")
	if err == nil {
		t.Fatal("ReadFile: want bounded error for 4 GiB file in a tiny image, got nil")
	}
	if !errors.Is(err, ErrCorrupt) || !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("ReadFile: err = %v; want ErrCorrupt + safeio.ErrTooLarge", err)
	}
}

// TestSecurity_HugeDirSize: a root directory whose extent Size is ~4 GiB must be
// rejected (bounded) when it is read, rather than allocating 4 GiB per lookup.
func TestSecurity_HugeDirSize(t *testing.T) {
	const blockSize = 2048
	img := minimalImage(10, 0x00, "")
	// Patch the root directory record inside the PVD (offset 156) to claim a
	// ~4 GiB extent size. parseDirRecord stores Size from bytes [10:14] LE.
	pvd := img[16*blockSize:]
	binary.LittleEndian.PutUint32(pvd[156+10:], 0xFFFFFFFF)

	fs, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		// Open may already fail while probing the primary SUSP; that is graceful.
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Open: unexpected error: %v", err)
		}
		return
	}
	_, err = fs.ListDir("/")
	if err == nil {
		t.Fatal("ListDir: want bounded error for 4 GiB dir extent, got nil")
	}
	if !errors.Is(err, ErrCorrupt) || !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("ListDir: err = %v; want ErrCorrupt + safeio.ErrTooLarge", err)
	}
}

// TestSecurity_SymlinkTraversalTarget: a Rock Ridge SL target containing ".."
// is returned verbatim by ReadLink (documented as caller-sanitised), and the
// in-driver resolver must NOT escape the image when such a link is traversed.
func TestSecurity_SymlinkTraversalTarget(t *testing.T) {
	// Without an SP entry in the root "." the driver does not enter Rock Ridge
	// mode, so ReadLink would not see the SL. Build an image that advertises RR
	// (SP on root ".") and carries the hostile SL on the file.
	img := rockRidgeImage("../../../../etc/passwd")

	fs, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// ReadLink returns the untrusted target verbatim (callers must sanitise).
	tgt, err := fs.ReadLink("/F.BIN")
	if err != nil {
		t.Fatalf("ReadLink: %v", err)
	}
	if tgt != "../../../../etc/passwd" {
		t.Fatalf("ReadLink target = %q; want the raw RR value", tgt)
	}
	// Resolving the link through the driver must stay inside the image: it can
	// only fail to find the target, never read host files. We assert it does
	// not panic and returns a not-found-style error.
	if _, err := fs.ReadFile("/F.BIN"); !errors.Is(err, ErrNotFound) {
		// A symlink to a nonexistent in-image path resolves to ErrNotFound; any
		// other non-nil error is also acceptable as long as it is not a panic.
		if err == nil {
			t.Fatal("ReadFile through ../ symlink: want error (target not in image), got nil")
		}
	}
}

// rockRidgeImage builds a minimal image whose root "." entry carries an SUSP SP
// (so the driver enters Rock Ridge mode) and whose single file carries a Rock
// Ridge SL with the given raw target.
func rockRidgeImage(slTarget string) []byte {
	const (
		blockSize   = 2048
		pvdLBA      = 16
		termLBA     = 17
		rootLBA     = 18
		totalBlocks = 20
	)
	img := make([]byte, totalBlocks*blockSize)

	pvd := img[pvdLBA*blockSize:]
	pvd[0] = vdTypePrimary
	copy(pvd[1:6], standardID)
	binary.LittleEndian.PutUint32(pvd[80:], totalBlocks)
	binary.LittleEndian.PutUint16(pvd[128:], blockSize)
	copy(pvd[156:], buildDirRecord(rootLBA, blockSize, flagDirectory, []byte{0x00}))

	term := img[termLBA*blockSize:]
	term[0] = vdTypeTerminator
	copy(term[1:6], standardID)

	root := img[rootLBA*blockSize:]
	pos := 0
	put := func(rec []byte) {
		copy(root[pos:], rec)
		pos += len(rec)
	}
	// Root "." with an SP entry (SUSP signal: bytes 0xBE 0xEF, LEN_SKP=0).
	dot := buildDirRecord(rootLBA, blockSize, flagDirectory, []byte{0x00})
	sp := []byte{'S', 'P', 7, 1, 0xBE, 0xEF, 0x00}
	dot = append(dot, sp...)
	dot[0] = byte(len(dot))
	put(dot)
	put(buildDirRecord(rootLBA, blockSize, flagDirectory, []byte{0x01})) // ".."

	name := []byte("F.BIN;1")
	rec := buildDirRecord(19, 0, 0x00, name)
	rec = appendSL(rec, slTarget)
	put(rec)
	return img
}

// TestSecurity_UnknownSizeBounded: when Open is called with size = -1 (unknown)
// a 4 GiB Size field must still be bounded by the fallback ceiling, not OOM.
func TestSecurity_UnknownSizeBounded(t *testing.T) {
	img := minimalImage(0xFFFFFFFF, 0x00, "")
	fs, err := Open(bytes.NewReader(img), -1)
	if err != nil {
		t.Fatalf("Open(size=-1): %v", err)
	}
	_, err = fs.ReadFile("/F.BIN")
	if !errors.Is(err, ErrCorrupt) || !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("ReadFile(size unknown): err = %v; want bounded ErrCorrupt", err)
	}
}

// TestSecurity_OverrunExtentList: a merged multi-extent record whose extent
// sizes overrun the allocated buffer must be rejected by safeio.Slice instead
// of panicking with a slice-bounds error.
func TestSecurity_OverrunExtentList(t *testing.T) {
	const blockSize = 2048
	vol := &Volume{BlockSize: blockSize}
	rec := dirRecord{
		Size: 10, // buffer is 10 bytes...
		extents: []extent{
			{lba: 19, size: 10},
			{lba: 20, size: 10}, // ...but the extents claim 20 bytes total.
		},
	}
	img := make([]byte, 64*blockSize)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("readFile panicked on overrunning extent list: %v", r)
			}
		}()
		if _, err := readFile(bytes.NewReader(img), vol, rec, int64(len(img))); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("readFile: err = %v; want ErrCorrupt (bounded slice)", err)
		}
	}()
}

// TestSecurity_AssortedMalformed runs a batch of malformed images through every
// read entry point and requires graceful (non-panicking) behaviour throughout.
func TestSecurity_AssortedMalformed(t *testing.T) {
	cases := []struct {
		name string
		img  []byte
		size int64
	}{
		{"huge-file", minimalImage(0xFFFFFFFF, 0x00, ""), -1},
		{"huge-file-known", minimalImage(0xFFFFFFFF, 0x00, ""), 20 * 2048},
		{"sl-traversal", rockRidgeImage("../../../../etc/passwd"), 20 * 2048},
		{"sl-absolute", rockRidgeImage("/etc/passwd"), 20 * 2048},
		{"size-shorter-than-image", minimalImage(8, 0x00, ""), 17 * 2048},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertGraceful(t, c.img, c.size)
		})
	}
}

// FuzzOpen feeds arbitrary (and seeded-hostile) byte blobs through the full read
// surface and requires that nothing panics. The seeds include the exact attack
// vectors from the threat model: a 4 GiB file Size in a tiny image, a 4 GiB
// directory extent Size, and a Rock Ridge SL with a "../" traversal target.
func FuzzOpen(f *testing.F) {
	// Seed: a structurally valid minimal image (the happy path the fuzzer
	// mutates from).
	f.Add(minimalImage(8, 0x00, ""))
	// Seed: 4 GiB file Size in a tiny image.
	f.Add(minimalImage(0xFFFFFFFF, 0x00, ""))
	// Seed: 4 GiB directory extent Size (patched root record).
	hugeDir := minimalImage(8, 0x00, "")
	binary.LittleEndian.PutUint32(hugeDir[16*2048+156+10:], 0xFFFFFFFF)
	f.Add(hugeDir)
	// Seed: Rock Ridge SL with a "../" traversal target.
	f.Add(rockRidgeImage("../../../../etc/passwd"))
	// Seed: tiny / truncated blobs.
	f.Add([]byte{})
	f.Add(make([]byte, 17*2048))

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on fuzzed image (%d bytes): %v", len(data), r)
			}
		}()
		fs, err := Open(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		_, _ = fs.ListDir("/")
		_, _ = fs.ReadFile("/F.BIN")
		_, _ = fs.Stat("/F.BIN")
		_, _ = fs.ReadLink("/F.BIN")
		// Also exercise the unknown-size path.
		if fs2, err := Open(bytes.NewReader(data), -1); err == nil {
			_, _ = fs2.ListDir("/")
			_, _ = fs2.ReadFile("/F.BIN")
		}
	})
}
