// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import (
	"bytes"
	"fmt"
	"io"
)

// Sector / descriptor layout constants.
const (
	sectorSize        = 2048 // ISO 9660 logical sector size
	systemAreaSectors = 16   // first 16 sectors are the (ignored) system area
	vdTypePrimary     = 1
	vdTypeSupplement  = 2 // supplementary VD — Joliet when the escape says UCS-2
	vdTypeTerminator  = 255
)

var standardID = []byte("CD001")

// Joliet escape sequences (at SVD offset 88) for UCS-2 levels 1/2/3.
var jolietEscapes = [][]byte{[]byte("%/@"), []byte("%/C"), []byte("%/E")}

// Volume is the decoded volume-descriptor data the reader needs. It carries
// both the Primary tree root and, when present, the Joliet (UCS-2) tree root;
// the FS picks which to traverse.
type Volume struct {
	VolumeID   string
	BlockSize  uint32 // logical block size (usually 2048)
	SpaceSize  uint32 // volume space size in logical blocks
	pvdRoot    dirRecord
	jolietRoot dirRecord
	hasJoliet  bool
}

// readVolume scans the volume-descriptor set from sector 16, decoding the
// Primary Volume Descriptor and (if present) a Joliet supplementary descriptor.
func readVolume(rs io.ReaderAt) (*Volume, error) {
	buf := make([]byte, sectorSize)
	v := &Volume{}
	havePVD := false
	for sector := int64(systemAreaSectors); ; sector++ {
		if _, err := rs.ReadAt(buf, sector*sectorSize); err != nil {
			return nil, fmt.Errorf("iso9660: read volume descriptor @sector %d: %w", sector, err)
		}
		if !bytes.Equal(buf[1:6], standardID) {
			return nil, ErrBadDescriptor
		}
		switch buf[0] {
		case vdTypePrimary:
			v.VolumeID = trimSpace(buf[40:72])
			v.SpaceSize = le32(buf[80:])
			v.BlockSize = uint32(le16(buf[128:]))
			if v.BlockSize == 0 {
				v.BlockSize = sectorSize
			}
			rec, _, err := parseDirRecord(buf[156:])
			if err != nil {
				return nil, fmt.Errorf("iso9660: root dir record: %w", err)
			}
			v.pvdRoot = rec
			havePVD = true
		case vdTypeSupplement:
			if isJolietEscape(buf[88:120]) {
				rec, _, err := parseDirRecord(buf[156:])
				if err == nil {
					v.jolietRoot = rec
					v.hasJoliet = true
				}
			}
		case vdTypeTerminator:
			if !havePVD {
				return nil, ErrBadDescriptor
			}
			return v, nil
		}
	}
}

// isJolietEscape reports whether an SVD escape-sequence field begins with one
// of the Joliet UCS-2 escape sequences.
func isJolietEscape(esc []byte) bool {
	for _, e := range jolietEscapes {
		if len(esc) >= len(e) && bytes.Equal(esc[:len(e)], e) {
			return true
		}
	}
	return false
}

func trimSpace(b []byte) string {
	return string(bytes.TrimRight(b, " \x00"))
}
