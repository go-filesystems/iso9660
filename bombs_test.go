// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

// bombs_test.go — memory-bomb regression tests. Each road below is a few bytes
// of hostile image that used to reserve far more than the image holds; each test
// asserts runtime.MemStats.TotalAlloc grows by a small budget while the road is
// refused (or, with the size unknown, while the buffer grows only with the data
// that exists). The "before" figures were measured on v0.4.0.
package iso9660

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"testing"

	"github.com/go-volumes/safeio"
)

func allocated(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

const miB = 1 << 20

// patchRootSize rewrites the Size field of the PVD's embedded root directory
// record (offset 156+10 of the descriptor at sector 16).
func patchRootSize(img []byte, size uint32) {
	binary.LittleEndian.PutUint32(img[16*2048+156+10:], size)
}

// With the image size unknown, a forged length was honoured up to the 1 GiB
// fallback ceiling: a 40 KiB image declaring a 512 MiB file allocated 512 MiB
// (measured) before the short read failed.
func TestBombUnknownSizeFileGrowsWithData(t *testing.T) {
	img := minimalImage(512*miB, 0x00, "")
	fs, err := Open(bytes.NewReader(img), -1)
	if err != nil {
		t.Fatal(err)
	}
	n := allocated(func() {
		if _, err := fs.ReadFile("/F.BIN"); err == nil {
			t.Error("ReadFile succeeded on a file 12 000 times larger than the image")
		}
	})
	if n > 8*miB {
		t.Errorf("ReadFile of a forged 512 MiB length allocated %d bytes, want < 8 MiB", n)
	}
}

// The same for a directory extent (16 MiB is the directory ceiling, so the claim
// stays under it).
func TestBombUnknownSizeDirGrowsWithData(t *testing.T) {
	img := minimalImage(8, 0x00, "")
	patchRootSize(img, 12*miB)
	fs, err := Open(bytes.NewReader(img), -1)
	if err != nil {
		t.Fatal(err)
	}
	n := allocated(func() {
		if _, err := fs.ListDir("/"); err == nil {
			t.Error("ListDir succeeded over a 12 MiB extent in a 40 KiB image")
		}
	})
	if n > 8*miB {
		t.Errorf("ListDir of a forged 12 MiB extent allocated %d bytes, want < 8 MiB", n)
	}
}

// With the size unknown the buffer grows geometrically (1 MiB, 2 MiB, ...) and
// the bytes must still come out right across every growth step and extent
// boundary.
func TestUnknownSizeReadIsExact(t *testing.T) {
	const fileLen = 5*miB + 123
	img := minimalImage(fileLen, 0x00, "")
	// The file starts at LBA 19; extend the image so that it is fully backed.
	img = append(img, make([]byte, fileLen)...)
	for i := 19 * 2048; i < len(img); i++ {
		img[i] = byte(i * 7)
	}
	want := img[19*2048 : 19*2048+fileLen]
	for _, size := range []int64{-1, int64(len(img))} {
		fs, err := Open(bytes.NewReader(img), size)
		if err != nil {
			t.Fatal(err)
		}
		got, err := fs.ReadFile("/F.BIN")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("size %d: ReadFile len %d, err %v, equal %v", size, len(got), err, bytes.Equal(got, want))
		}
	}
}

// With the size known, a record whose extent lies outside the image is refused
// before anything is reserved (v0.4.0 reserved Size, then failed the read).
func TestBombKnownSizeExtentOutsideImage(t *testing.T) {
	// 6 MiB image, a 4 MiB file whose extent starts 3 MiB from the end.
	img := minimalImage(4*miB, 0x00, "")
	img = append(img, make([]byte, 6*miB-len(img))...)
	fs, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	// The file sits at LBA 19 (offset 38912) and is 4 MiB: it fits in the 6 MiB
	// image. Declare a size that still covers Size but ends inside the extent.
	fs.size = 4*miB + 1000
	n := allocated(func() {
		if _, err := fs.ReadFile("/F.BIN"); !errors.Is(err, ErrCorrupt) {
			t.Errorf("err = %v, want ErrCorrupt", err)
		}
	})
	if n > miB {
		t.Errorf("allocated %d bytes for an extent outside the image, want < 1 MiB", n)
	}
}

// A directory's size is a u32, but a directory of more than 16 MiB is not a
// directory anyone wrote. Over a real 20 MiB image, a root record claiming
// 16 MiB + 1 was accepted and reserved (measured: 16 MiB per lookup). Exact:
// 16 MiB lists, one byte more is refused before the reservation.
func TestBombDirExtentCeiling(t *testing.T) {
	img := minimalImage(8, 0x00, "")
	img = append(img, make([]byte, 20*miB-len(img))...)

	patchRootSize(img, maxDirExtent)
	fs, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ListDir("/"); err != nil {
		t.Fatalf("directory of exactly %d bytes: %v", maxDirExtent, err)
	}

	patchRootSize(img, maxDirExtent+1)
	fs, err = Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	n := allocated(func() {
		if _, err := fs.ListDir("/"); !errors.Is(err, ErrCorrupt) || !errors.Is(err, safeio.ErrTooLarge) {
			t.Errorf("err = %v, want ErrCorrupt + safeio.ErrTooLarge", err)
		}
	})
	if n > miB {
		t.Errorf("allocated %d bytes for an over-ceiling directory, want < 1 MiB", n)
	}
}

type countingReaderAt struct {
	r     io.ReaderAt
	reads int
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.reads++
	return c.r.ReadAt(p, off)
}

// The descriptor scan stopped only at a terminator: an image of descriptors
// that never end was read to its last byte. The reference reader scans sectors
// 16..99 (linux fs/isofs/inode.c). Exact: a terminator at sector 99 is found,
// at sector 100 it is not.
func TestVolumeDescriptorScanBounded(t *testing.T) {
	build := func(termSector int) []byte {
		img := make([]byte, 300*2048)
		for s := 16; s < 300; s++ {
			d := img[s*2048:]
			d[0] = 2 // supplementary, no Joliet escape: ignored
			copy(d[1:6], standardID)
		}
		pvd := img[16*2048:]
		pvd[0] = vdTypePrimary
		binary.LittleEndian.PutUint16(pvd[128:], 2048)
		copy(pvd[156:], buildDirRecord(200, 2048, flagDirectory, []byte{0x00}))
		img[termSector*2048] = vdTypeTerminator
		return img
	}
	if _, err := readVolume(bytes.NewReader(build(99))); err != nil {
		t.Errorf("terminator at sector 99: %v", err)
	}
	c := &countingReaderAt{r: bytes.NewReader(build(100))}
	if _, err := readVolume(c); !errors.Is(err, ErrBadDescriptor) {
		t.Errorf("terminator at sector 100: err = %v, want ErrBadDescriptor", err)
	}
	if c.reads > 84 {
		t.Errorf("scanned %d sectors, want at most 84", c.reads)
	}
}

// FuzzBombs opens arbitrary bytes with the size known and unknown and walks the
// read surface. Property: no panic, and no road allocates more than a fixed
// budget plus a multiple of the input (a 16 MiB directory parses into a few
// hundred MiB at worst; fuzz inputs are tiny, so the budget is what binds).
func FuzzBombs(f *testing.F) {
	f.Add(minimalImage(8, 0x00, ""))
	f.Add(minimalImage(512*miB, 0x00, ""))
	huge := minimalImage(8, 0x00, "")
	patchRootSize(huge, 12*miB)
	f.Add(huge)
	f.Add(rockRidgeImage("../x"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, size := range []int64{int64(len(data)), -1} {
			n := allocated(func() {
				fs, err := Open(bytes.NewReader(data), size)
				if err != nil {
					return
				}
				_, _ = fs.ListDir("/")
				_, _ = fs.ReadFile("/F.BIN")
				_, _ = fs.ReadLink("/F.BIN")
				if file, err := fs.OpenFile("/F.BIN"); err == nil {
					buf := make([]byte, 4096)
					_, _ = file.ReadAt(buf, 0)
					file.Close()
				}
			})
			if limit := uint64(96*miB) + 64*uint64(len(data)); n > limit {
				t.Fatalf("size %d: %d bytes allocated for a %d-byte image", size, n, len(data))
			}
		}
	})
}
