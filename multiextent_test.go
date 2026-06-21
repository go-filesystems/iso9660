// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// buildDirRecord assembles a single ISO 9660 directory record exactly as
// parseDirRecord decodes it: length at [0], extent LBA (LE) at [2:6], data
// length (LE) at [10:14], file flags at [25], name length at [32] and the file
// identifier at [33:]. A pad byte follows an even-length name (16-bit field
// alignment); no System Use Area is emitted here.
func buildDirRecord(extentLBA, size uint32, flags byte, name []byte) []byte {
	recLen := 33 + len(name)
	if len(name)%2 == 0 {
		recLen++ // alignment pad after an even-length name
	}
	rec := make([]byte, recLen)
	rec[0] = byte(recLen)
	binary.LittleEndian.PutUint32(rec[2:], extentLBA)
	binary.LittleEndian.PutUint32(rec[10:], size)
	rec[25] = flags
	rec[32] = byte(len(name))
	copy(rec[33:], name)
	return rec
}

// TestMultiExtentReadFile verifies the multi-extent accumulation logic on a
// fully synthetic, hand-built ISO 9660 image. One file ("BIG.BIN") is recorded
// as two consecutive directory records sharing the same identifier: the first
// carries the multi-extent flag (flagMultiExt) and points at extent A, the
// second clears the flag and points at extent B. ReadFile must return A||B with
// the summed size, and the file must surface as a single directory entry.
//
// NOTE: the fixture is synthetic. genisoimage/xorriso are not available in this
// environment, so this exercises the accumulation/merging logic for byte-exact
// correctness; it is not a cross-validated parity check against a real mastering
// tool's >4 GiB multi-extent output.
func TestMultiExtentReadFile(t *testing.T) {
	const (
		blockSize = 2048
		// Sector map (in logical blocks):
		//  0..15 system area
		//  16    primary volume descriptor
		//  17    volume-descriptor set terminator
		//  18    root directory extent
		//  19    file extent A
		//  20    file extent B
		pvdLBA      = 16
		termLBA     = 17
		rootLBA     = 18
		extentALBA  = 19
		extentBLBA  = 20
		totalBlocks = 21
	)

	img := make([]byte, totalBlocks*blockSize)

	// --- Primary Volume Descriptor (sector 16) ---
	pvd := img[pvdLBA*blockSize:]
	pvd[0] = vdTypePrimary
	copy(pvd[1:6], standardID)
	binary.LittleEndian.PutUint32(pvd[80:], totalBlocks) // volume space size
	binary.LittleEndian.PutUint16(pvd[128:], blockSize)  // logical block size
	// Root directory record lives at PVD offset 156; it is a directory whose
	// extent is the root directory at rootLBA. Name is the single byte 0x00.
	copy(pvd[156:], buildDirRecord(rootLBA, blockSize, flagDirectory, []byte{0x00}))

	// --- Volume-descriptor set terminator (sector 17) ---
	term := img[termLBA*blockSize:]
	term[0] = vdTypeTerminator
	copy(term[1:6], standardID)

	// --- Root directory extent (sector 18) ---
	// Contains "." , ".." and the two consecutive records for BIG.BIN;1.
	root := img[rootLBA*blockSize:]
	pos := 0
	put := func(rec []byte) {
		copy(root[pos:], rec)
		pos += len(rec)
	}
	put(buildDirRecord(rootLBA, blockSize, flagDirectory, []byte{0x00})) // "."
	put(buildDirRecord(rootLBA, blockSize, flagDirectory, []byte{0x01})) // ".."

	name := []byte("BIG.BIN;1")
	dataA := bytes.Repeat([]byte{0xAA}, 1500)
	dataB := bytes.Repeat([]byte{0xBB}, 700)
	// First record: multi-extent flag set, extent A.
	put(buildDirRecord(extentALBA, uint32(len(dataA)), flagMultiExt, name))
	// Second (final) record: flag clear, extent B.
	put(buildDirRecord(extentBLBA, uint32(len(dataB)), 0x00, name))

	// --- File data extents ---
	copy(img[extentALBA*blockSize:], dataA)
	copy(img[extentBLBA*blockSize:], dataB)

	fs, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// The multi-extent file must appear as a single entry.
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "BIG.BIN" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("ListDir(/) = %v; want exactly [BIG.BIN]", names)
	}

	// Stat reports the summed size.
	wantSize := uint64(len(dataA) + len(dataB))
	if st, err := fs.Stat("/BIG.BIN"); err != nil {
		t.Errorf("Stat(/BIG.BIN): %v", err)
	} else if st.Size() != wantSize {
		t.Errorf("Stat(/BIG.BIN) size = %d, want %d", st.Size(), wantSize)
	}

	// ReadFile returns extent A followed by extent B.
	got, err := fs.ReadFile("/BIG.BIN")
	if err != nil {
		t.Fatalf("ReadFile(/BIG.BIN): %v", err)
	}
	want := append(append([]byte(nil), dataA...), dataB...)
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadFile(/BIG.BIN): %d bytes, content mismatch (want %d)", len(got), len(want))
	}
}

// TestMultiExtentMissingFinal checks that a multi-extent run with no final
// (flag-clear) record is rejected as corrupt rather than silently truncated.
func TestMultiExtentMissingFinal(t *testing.T) {
	name := []byte("BAD.BIN;1")
	in := []dirRecord{
		mustParse(t, buildDirRecord(19, 100, flagMultiExt, name)),
		mustParse(t, buildDirRecord(20, 100, flagMultiExt, name)),
	}
	if _, err := mergeMultiExtent(in); err == nil {
		t.Fatal("mergeMultiExtent: want error for run without final extent, got nil")
	}
}

func mustParse(t *testing.T, b []byte) dirRecord {
	t.Helper()
	rec, _, err := parseDirRecord(b)
	if err != nil {
		t.Fatalf("parseDirRecord: %v", err)
	}
	return rec
}
