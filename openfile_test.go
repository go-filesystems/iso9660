// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// probeOpener asserts the capability is reachable the way a caller reaches it —
// through the filesystem.Filesystem interface, not the concrete *FS.
func probeOpener(t *testing.T, fs *FS) filesystem.Opener {
	t.Helper()
	var generic filesystem.Filesystem = fs
	o, ok := generic.(filesystem.Opener)
	if !ok {
		t.Fatal("iso9660 does not satisfy filesystem.Opener")
	}
	return o
}

// checkAgainstReadFile is the verification that matters: for a file on a real
// image, every ReadAt must return EXACTLY the corresponding slice of what
// ReadFile returns. Offsets are swept around each value in bounds — the extent
// boundaries and the logical-block boundaries — because ISO extent lengths are
// byte counts and do NOT divide evenly by the block size, so a mapping that
// assumed they did would be invisible on a single-extent file and wrong on
// every multi-extent one.
func checkAgainstReadFile(t *testing.T, fs *FS, path string, bounds []int64) {
	t.Helper()
	want, err := fs.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	f, err := probeOpener(t, fs).OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", path, err)
	}
	defer f.Close()

	size := int64(len(want))
	if f.Size() != size {
		t.Fatalf("%s: Size() = %d, want %d (len of ReadFile)", path, f.Size(), size)
	}

	offsets := map[int64]bool{0: true, size: true}
	if size > 0 {
		offsets[size-1] = true
	}
	for _, b := range bounds {
		for _, o := range []int64{b - 1, b, b + 1} {
			if o >= 0 && o <= size {
				offsets[o] = true
			}
		}
	}
	lengths := []int{1, 3, 512, 2047, 2048, 2049, 5000}

	for off := range offsets {
		for _, l := range lengths {
			p := make([]byte, l)
			n, err := f.ReadAt(p, off)

			end := off + int64(l)
			short := end > size
			wantN := l
			if short {
				wantN = int(size - off)
			}
			if n != wantN {
				t.Fatalf("%s: ReadAt(len=%d, off=%d) n = %d, want %d", path, l, off, n, wantN)
			}
			if short {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("%s: ReadAt(len=%d, off=%d) err = %v, want io.EOF", path, l, off, err)
				}
			} else if err != nil {
				t.Fatalf("%s: ReadAt(len=%d, off=%d) err = %v, want nil", path, l, off, err)
			}
			if !bytes.Equal(p[:n], want[off:off+int64(n)]) {
				t.Fatalf("%s: ReadAt(len=%d, off=%d) bytes differ from ReadFile[%d:%d]", path, l, off, off, off+int64(n))
			}
		}
	}

	// io.SectionReader is the consumer the io.ReaderAt contract protects.
	got, err := io.ReadAll(io.NewSectionReader(f, 0, size))
	if err != nil {
		t.Fatalf("%s: ReadAll(SectionReader): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: SectionReader round-trip differs from ReadFile", path)
	}
}

// TestOpenFileMultiExtentFixture is the strongest real-image proof this driver
// can make. The embedded fixture is a real ECMA-119 byte stream whose BIG.BIN
// is recorded as THREE extents of 1500, 2048 and 700 bytes: the file-offset
// boundaries fall at 1500 and 3548, neither of which is a multiple of the
// 2048-byte logical block. Any ReadAt that assumed uniform blocks — or that
// walked the extent list off by one — produces wrong bytes here and matching
// bytes on any single-extent file.
func TestOpenFileMultiExtentFixture(t *testing.T) {
	fs, err := Open(bytes.NewReader(multiExtentISO), int64(len(multiExtentISO)))
	if err != nil {
		t.Fatalf("Open embedded fixture: %v", err)
	}
	defer fs.Close()

	// Extent boundaries and block boundaries, both swept.
	bounds := []int64{1500, 3548, 2048, 4096}
	checkAgainstReadFile(t, fs, "/BIG.BIN", bounds)
	checkAgainstReadFile(t, fs, "/PLAIN.TXT", []int64{2048})

	// Spot-check the extent boundary explicitly: a read straddling 1500 must
	// stitch the 0xAA extent to the 0xBB one with no gap and no repeat.
	f, err := probeOpener(t, fs).OpenFile("/BIG.BIN")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()
	p := make([]byte, 4)
	if n, err := f.ReadAt(p, 1498); n != 4 || err != nil {
		t.Fatalf("ReadAt across extent boundary = %d, %v", n, err)
	}
	if !bytes.Equal(p, []byte{0xAA, 0xAA, 0xBB, 0xBB}) {
		t.Fatalf("across extent boundary 1500 got % x, want AA AA BB BB", p)
	}
	if n, err := f.ReadAt(p, 3546); n != 4 || err != nil {
		t.Fatalf("ReadAt across extent boundary 3548 = %d, %v", n, err)
	}
	if !bytes.Equal(p, []byte{0xBB, 0xBB, 0xCC, 0xCC}) {
		t.Fatalf("across extent boundary 3548 got % x, want BB BB CC CC", p)
	}
}

// TestOpenFileBuiltImage repeats the proof on an image this package masters,
// with a file spanning several logical blocks.
func TestOpenFileBuiltImage(t *testing.T) {
	b := NewBuilder("OPENERTEST")
	body := pattern(5000) // > 2 logical blocks
	if err := b.AddDir("/SUB"); err != nil {
		t.Fatalf("AddDir: %v", err)
	}
	if err := b.AddFile("/DATA.BIN", body); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	if err := b.AddFile("/SUB/NESTED.BIN", pattern(2048)); err != nil {
		t.Fatalf("AddFile nested: %v", err)
	}
	if err := b.AddFile("/EMPTY.BIN", nil); err != nil {
		t.Fatalf("AddFile empty: %v", err)
	}
	fs, _ := buildImage(t, b)
	defer fs.Close()

	checkAgainstReadFile(t, fs, "/DATA.BIN", []int64{2048, 4096})
	checkAgainstReadFile(t, fs, "/SUB/NESTED.BIN", []int64{2048})

	// An empty file: Size 0 and any read is immediately io.EOF.
	f, err := probeOpener(t, fs).OpenFile("/EMPTY.BIN")
	if err != nil {
		t.Fatalf("OpenFile empty: %v", err)
	}
	defer f.Close()
	if f.Size() != 0 {
		t.Fatalf("Size() on empty = %d, want 0", f.Size())
	}
	if n, err := f.ReadAt(make([]byte, 4), 0); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt on empty = %d, %v; want 0, io.EOF", n, err)
	}
}

// TestInterop_GenisoimageOpenFile masters an image with the real genisoimage /
// mkisofs and checks ReadAt against ReadFile on it. This is the cross-tool
// proof: the layout is somebody else's. Skipped when the tool is absent (CI
// installs it).
func TestInterop_GenisoimageOpenFile(t *testing.T) {
	tool := findTool("genisoimage")
	if tool == "" {
		tool = findTool("mkisofs")
	}
	if tool == "" {
		if tool = findTool("xorriso"); tool != "" {
			// xorriso's mkisofs-compatible personality takes the same flags.
			t.Logf("using xorriso -as mkisofs")
		}
	}
	if tool == "" {
		t.Skip("genisoimage/mkisofs/xorriso not available — skipping iso9660 OpenFile interop test")
	}

	src := t.TempDir()
	big := pattern(70000) // ~34 logical blocks
	small := []byte("small file\n")
	for name, content := range map[string][]byte{
		"BIG.BIN":        big,
		"SMALL.TXT":      small,
		"SUB/NESTED.BIN": pattern(4096), // exactly two blocks
	} {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	iso := filepath.Join(t.TempDir(), "out.iso")
	var cmd *exec.Cmd
	if filepath.Base(tool) == "xorriso" {
		cmd = exec.Command(tool, "-as", "mkisofs", "-quiet", "-o", iso, src)
	} else {
		cmd = exec.Command(tool, "-quiet", "-o", iso, src)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("%s failed (%v): %s", tool, err, out)
	}

	fs, err := OpenFile(iso)
	if err != nil {
		t.Fatalf("OpenFile(iso): %v", err)
	}
	defer fs.Close()

	bounds := []int64{2048, 4096, 8192, 65536}
	checkAgainstReadFile(t, fs, "/BIG.BIN", bounds)
	checkAgainstReadFile(t, fs, "/SMALL.TXT", []int64{2048})
	checkAgainstReadFile(t, fs, "/SUB/NESTED.BIN", []int64{2048, 4096})

	// And the bytes really are the ones on the host before mastering.
	f, err := probeOpener(t, fs).OpenFile("/BIG.BIN")
	if err != nil {
		t.Fatalf("OpenFile(/BIG.BIN): %v", err)
	}
	defer f.Close()
	got := make([]byte, 3000)
	if n, err := f.ReadAt(got, 30000); n != 3000 || err != nil {
		t.Fatalf("ReadAt(3000, 30000) = %d, %v", n, err)
	}
	if !bytes.Equal(got, big[30000:33000]) {
		t.Fatal("ReadAt bytes differ from the file mastered into the image")
	}
}

// TestOpenFileEOFSemantics pins the io.ReaderAt end-of-file rules. A short read
// with a nil error is the failure mode that breaks io.SectionReader silently.
func TestOpenFileEOFSemantics(t *testing.T) {
	fs, err := Open(bytes.NewReader(multiExtentISO), int64(len(multiExtentISO)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	f, err := probeOpener(t, fs).OpenFile("/PLAIN.TXT")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	const body = "ordinary single-extent sibling\n"
	if f.Size() != int64(len(body)) {
		t.Fatalf("Size() = %d, want %d", f.Size(), len(body))
	}
	p := make([]byte, 5)
	if n, err := f.ReadAt(p, 9); n != 5 || err != nil || string(p) != "singl" {
		t.Fatalf("ReadAt(5,9) = %d, %v, %q", n, err, p)
	}
	// Straddling the end: bytes AND io.EOF.
	off := int64(len(body)) - 2
	if n, err := f.ReadAt(p, off); n != 2 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt straddling end = %d, %v; want 2, io.EOF", n, err)
	}
	// At and past Size(): 0, io.EOF.
	if n, err := f.ReadAt(p, int64(len(body))); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt at Size() = %d, %v; want 0, io.EOF", n, err)
	}
	if n, err := f.ReadAt(p, 1<<40); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt past Size() = %d, %v; want 0, io.EOF", n, err)
	}
	// Zero-length read inside the file is a full read.
	if n, err := f.ReadAt(nil, 3); n != 0 || err != nil {
		t.Fatalf("ReadAt(empty,3) = %d, %v; want 0, nil", n, err)
	}
	// Negative offset errors instead of panicking.
	if n, err := f.ReadAt(p, -1); n != 0 || err == nil {
		t.Fatalf("ReadAt(-1) = %d, %v; want an error", n, err)
	}
	// Close is idempotent and a read after it fails loudly.
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if n, err := f.ReadAt(p, 0); n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("ReadAt after Close = %d, %v; want 0, os.ErrClosed", n, err)
	}
}

// TestOpenFileRejects covers the refusal paths: a directory, and a path that
// does not resolve. Each fails the way ReadFile does.
func TestOpenFileRejects(t *testing.T) {
	b := NewBuilder("REJECT")
	if err := b.AddDir("/SUB"); err != nil {
		t.Fatalf("AddDir: %v", err)
	}
	if err := b.AddFile("/SUB/F.BIN", []byte("x")); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	fs, _ := buildImage(t, b)
	defer fs.Close()
	o := probeOpener(t, fs)

	if _, err := o.OpenFile("/SUB"); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("OpenFile(dir) = %v, want ErrNotRegular", err)
	}
	if _, err := o.OpenFile("/"); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("OpenFile(/) = %v, want ErrNotRegular", err)
	}
	if _, err := o.OpenFile("/NOPE.BIN"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("OpenFile(missing) = %v, want ErrNotFound", err)
	}
}

// TestOpenFileConcurrentReads exercises the concurrency guarantee io.ReaderAt
// makes and a mount depends on. Under -race this is what would catch shared
// mutable state on the read path.
func TestOpenFileConcurrentReads(t *testing.T) {
	fs, err := Open(bytes.NewReader(multiExtentISO), int64(len(multiExtentISO)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	want, err := fs.ReadFile("/BIG.BIN")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	f, err := probeOpener(t, fs).OpenFile("/BIG.BIN")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	var wg sync.WaitGroup
	errCh := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64(i) * 137
			if off > f.Size() {
				off = f.Size()
			}
			p := make([]byte, 1600+i)
			n, err := f.ReadAt(p, off)
			if err != nil && !errors.Is(err, io.EOF) {
				errCh <- fmt.Errorf("goroutine %d: %w", i, err)
				return
			}
			if !bytes.Equal(p[:n], want[off:off+int64(n)]) {
				errCh <- fmt.Errorf("goroutine %d: bytes differ at off=%d", i, off)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// --- forged / corrupt records ---------------------------------------------
//
// openRecord is exercised directly with hand-built records, the way the
// multi-extent tests build them: these shapes cannot come out of the parser
// today, and the point is that the offset path stays as defensive as readFile
// if one ever does.

// openTestFS returns an FS over the embedded fixture, for record-level tests.
func openTestFS(t *testing.T) *FS {
	t.Helper()
	fs, err := Open(bytes.NewReader(multiExtentISO), int64(len(multiExtentISO)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return fs
}

// TestOpenRecordExtentsOverrunSize covers the guard that mirrors readFile's
// safeio.Slice bound: an extent list summing past the declared Size must be
// rejected, not silently expose bytes past the end.
func TestOpenRecordExtentsOverrunSize(t *testing.T) {
	fs := openTestFS(t)
	defer fs.Close()
	rec := dirRecord{
		Size:    100,
		extents: []extent{{lba: 19, size: 60}, {lba: 20, size: 60}},
	}
	if _, err := fs.openRecord(rec); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("openRecord(overrunning extents) = %v, want ErrCorrupt", err)
	}
	// readFile refuses the same record, so the two paths agree.
	if _, err := readFile(fs.rs, fs.vol, rec, allocCeiling(fs.size)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("readFile(overrunning extents) = %v, want ErrCorrupt", err)
	}
}

// TestOpenRecordExtentsUnderrunSize covers the other direction: extents that
// fall short of Size. readFile allocates Size and leaves the tail zeroed, so
// ReadAt must serve zeros there — byte-for-byte the same file.
func TestOpenRecordExtentsUnderrunSize(t *testing.T) {
	fs := openTestFS(t)
	defer fs.Close()
	rec := dirRecord{
		Size: 200,
		// LBA 19 holds 1500×0xAA in the fixture; take 50 of them, and a
		// zero-length extent that must be skipped exactly as readFile skips it.
		extents: []extent{{lba: 19, size: 50}, {lba: 20, size: 0}},
	}
	want, err := readFile(fs.rs, fs.vol, rec, allocCeiling(fs.size))
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	f, err := fs.openRecord(rec)
	if err != nil {
		t.Fatalf("openRecord: %v", err)
	}
	defer f.Close()
	if f.Size() != 200 {
		t.Fatalf("Size() = %d, want 200", f.Size())
	}
	got := make([]byte, 200)
	if n, err := f.ReadAt(got, 0); n != 200 || err != nil {
		t.Fatalf("ReadAt(all) = %d, %v; want 200, nil", n, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("ReadAt over an under-covered record differs from ReadFile")
	}
	// A read starting inside the zero tail is still a normal read.
	tail := make([]byte, 10)
	if n, err := f.ReadAt(tail, 100); n != 10 || err != nil {
		t.Fatalf("ReadAt in zero tail = %d, %v", n, err)
	}
	if !bytes.Equal(tail, make([]byte, 10)) {
		t.Fatalf("zero tail read % x, want zeros", tail)
	}
}

// TestOpenRecordSizeExceedsImage covers the corruption check: a data length
// larger than the whole image cannot be a real file.
func TestOpenRecordSizeExceedsImage(t *testing.T) {
	fs := openTestFS(t)
	defer fs.Close()
	rec := dirRecord{ExtentLBA: 19, Size: uint32(len(multiExtentISO)) + 1}
	if _, err := fs.openRecord(rec); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("openRecord(oversized) = %v, want ErrCorrupt", err)
	}
	// With an unknown image size there is nothing to compare against and no
	// allocation to bound, so the record opens; a bogus extent then surfaces
	// as a read error rather than a refusal to open.
	unknown, err := Open(bytes.NewReader(multiExtentISO), -1)
	if err != nil {
		t.Fatalf("Open(size=-1): %v", err)
	}
	defer unknown.Close()
	f, err := unknown.openRecord(rec)
	if err != nil {
		t.Fatalf("openRecord(size unknown) = %v, want success", err)
	}
	defer f.Close()
	if f.Size() != int64(rec.Size) {
		t.Fatalf("Size() = %d, want %d", f.Size(), rec.Size)
	}
}

// failingReaderAt fails every read at or past failFrom, so a data-read error
// can be injected at an exact image offset.
type failingReaderAt struct {
	inner    io.ReaderAt
	failFrom int64
}

func (r failingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.failFrom {
		return 0, io.ErrUnexpectedEOF
	}
	return r.inner.ReadAt(p, off)
}

// TestOpenFileReadError covers the I/O error branch of ReadAt: the error must
// come back, wrapped, with however many bytes were already delivered — never
// as a silent short read.
func TestOpenFileReadError(t *testing.T) {
	const blockSize = 2048
	// The fixture's BIG.BIN extents are LBA 19, 20, 21; fail from LBA 20 on,
	// so the first extent (1500 bytes) is served and the second fails.
	rs := failingReaderAt{inner: bytes.NewReader(multiExtentISO), failFrom: 20 * blockSize}
	fs, err := Open(bytes.NewReader(multiExtentISO), int64(len(multiExtentISO)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	rec, err := fs.resolve("/BIG.BIN")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	fs.rs = rs // swap the backing reader for the injected-failure one
	f, err := fs.openRecord(rec)
	if err != nil {
		t.Fatalf("openRecord: %v", err)
	}
	defer f.Close()

	p := make([]byte, 2000)
	n, err := f.ReadAt(p, 0)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadAt err = %v, want io.ErrUnexpectedEOF", err)
	}
	if n != 1500 {
		t.Fatalf("ReadAt n = %d, want 1500 (first extent delivered)", n)
	}
}
