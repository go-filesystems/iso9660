// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

//go:build ignore

// Command gen_multiextent regenerates testdata/multiextent.iso, the hand-built
// ECMA-119 fixture embedded by the multi-extent read tests.
//
// A real mastering tool (mkisofs/xorriso) only records a file across multiple
// directory records — the multi-extent form of ECMA-119 §6.5.1 — when the file
// exceeds a single extent's 4 GiB byte-length field, which would make any
// genuine fixture larger than 4 GiB and impossible to embed. The image is
// therefore hand-built here, yet is a real on-disk ISO byte stream that the
// driver parses end-to-end from Open through the volume descriptors and
// directory records to the file extents.
//
// Layout: BIG.BIN is three consecutive directory records (two carrying the
// multi-extent flag, one final) whose extents concatenate to A||B||C; PLAIN.TXT
// is an ordinary single-extent sibling that immediately follows the run, so the
// fixture also proves the merge stops at the final extent rather than swallowing
// the next entry.
//
// Regenerate with:  go run testdata/gen_multiextent.go testdata/multiextent.iso
package main

import (
	"encoding/binary"
	"os"
)

const (
	blockSize     = 2048
	vdTypePrimary = 1
	vdTypeTerm    = 255
	flagDir       = 0x02
	flagMultiExt  = 0x80
)

var standardID = []byte("CD001")

func buildDirRecord(extentLBA, size uint32, flags byte, name []byte) []byte {
	recLen := 33 + len(name)
	if len(name)%2 == 0 {
		recLen++
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

func rep(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func main() {
	const (
		pvdLBA    = 16
		termLBA   = 17
		rootLBA   = 18
		extALBA   = 19
		extBLBA   = 20
		extCLBA   = 21
		plainLBA  = 22
		totalBlks = 23
	)
	img := make([]byte, totalBlks*blockSize)

	pvd := img[pvdLBA*blockSize:]
	pvd[0] = vdTypePrimary
	copy(pvd[1:6], standardID)
	copy(pvd[40:72], []byte("MULTIEXTENT_FIXTURE"))
	binary.LittleEndian.PutUint32(pvd[80:], totalBlks)
	binary.LittleEndian.PutUint16(pvd[128:], blockSize)
	copy(pvd[156:], buildDirRecord(rootLBA, blockSize, flagDir, []byte{0x00}))

	term := img[termLBA*blockSize:]
	term[0] = vdTypeTerm
	copy(term[1:6], standardID)

	root := img[rootLBA*blockSize:]
	pos := 0
	put := func(rec []byte) {
		copy(root[pos:], rec)
		pos += len(rec)
	}
	put(buildDirRecord(rootLBA, blockSize, flagDir, []byte{0x00})) // "."
	put(buildDirRecord(rootLBA, blockSize, flagDir, []byte{0x01})) // ".."

	big := []byte("BIG.BIN;1")
	dataA := rep(0xAA, 1500)
	dataB := rep(0xBB, 2048) // exactly one sector
	dataC := rep(0xCC, 700)
	put(buildDirRecord(extALBA, uint32(len(dataA)), flagMultiExt, big)) // extent 1/3
	put(buildDirRecord(extBLBA, uint32(len(dataB)), flagMultiExt, big)) // extent 2/3
	put(buildDirRecord(extCLBA, uint32(len(dataC)), 0x00, big))         // final extent

	plain := []byte("PLAIN.TXT;1")
	dataP := []byte("ordinary single-extent sibling\n")
	put(buildDirRecord(plainLBA, uint32(len(dataP)), 0x00, plain))

	copy(img[extALBA*blockSize:], dataA)
	copy(img[extBLBA*blockSize:], dataB)
	copy(img[extCLBA*blockSize:], dataC)
	copy(img[plainLBA*blockSize:], dataP)

	if len(os.Args) < 2 {
		panic("usage: go run testdata/gen_multiextent.go <output.iso>")
	}
	if err := os.WriteFile(os.Args[1], img, 0o644); err != nil {
		panic(err)
	}
}
