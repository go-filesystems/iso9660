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
	sectorSize       = 2048 // ISO 9660 logical sector size
	systemAreaSectors = 16  // first 16 sectors are the (ignored) system area
	vdTypePrimary    = 1
	vdTypeTerminator = 255
)

var standardID = []byte("CD001")

// Volume is the decoded Primary Volume Descriptor data the reader needs.
type Volume struct {
	VolumeID      string
	BlockSize     uint32 // logical block size from the PVD (usually 2048)
	SpaceSize     uint32 // volume space size in logical blocks
	Root          dirRecord
}

// readVolume scans the volume-descriptor set starting at sector 16 and returns
// the Primary Volume Descriptor.
func readVolume(rs io.ReaderAt) (*Volume, error) {
	buf := make([]byte, sectorSize)
	for sector := int64(systemAreaSectors); ; sector++ {
		if _, err := rs.ReadAt(buf, sector*sectorSize); err != nil {
			return nil, fmt.Errorf("iso9660: read volume descriptor @sector %d: %w", sector, err)
		}
		if !bytes.Equal(buf[1:6], standardID) {
			return nil, ErrBadDescriptor
		}
		switch buf[0] {
		case vdTypePrimary:
			return parsePVD(buf)
		case vdTypeTerminator:
			return nil, ErrBadDescriptor
		}
		// Other descriptor types (boot, supplementary/Joliet) are skipped.
	}
}

// parsePVD decodes the fields of a Primary Volume Descriptor. Numeric fields
// are "both-endian" (little then big); we read the little-endian half.
func parsePVD(buf []byte) (*Volume, error) {
	v := &Volume{
		VolumeID:  trimSpace(buf[40:72]),
		SpaceSize: le32(buf[80:]),
		BlockSize: uint32(le16(buf[128:])),
	}
	if v.BlockSize == 0 {
		v.BlockSize = sectorSize
	}
	rec, _, err := parseDirRecord(buf[156:])
	if err != nil {
		return nil, fmt.Errorf("iso9660: root dir record: %w", err)
	}
	v.Root = rec
	return v, nil
}

func trimSpace(b []byte) string {
	return string(bytes.TrimRight(b, " \x00"))
}
