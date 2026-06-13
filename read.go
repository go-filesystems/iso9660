// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import (
	"fmt"
	"io"
)

// readDirRecords reads and parses the directory whose extent is described by
// rec. ISO 9660 directory records never span a logical sector, so a zero
// length byte means "skip to the next sector". The "." and ".." entries are
// included; callers filter them as needed.
func readDirRecords(rs io.ReaderAt, vol *Volume, rec dirRecord) ([]dirRecord, error) {
	if !rec.isDir() {
		return nil, ErrNotDirectory
	}
	bs := int(vol.BlockSize)
	data := make([]byte, rec.Size)
	if _, err := rs.ReadAt(data, int64(rec.ExtentLBA)*int64(bs)); err != nil {
		return nil, fmt.Errorf("iso9660: read dir extent @LBA %d: %w", rec.ExtentLBA, err)
	}

	var out []dirRecord
	pos := 0
	for pos < len(data) {
		if data[pos] == 0 {
			// Pad to the next sector boundary.
			next := (pos/bs + 1) * bs
			if next <= pos {
				break
			}
			pos = next
			continue
		}
		child, n, err := parseDirRecord(data[pos:])
		if err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
		out = append(out, child)
		pos += n
	}
	return out, nil
}

// readFile returns the full contents of the file described by rec. Base
// ISO 9660 files occupy a single contiguous extent.
func readFile(rs io.ReaderAt, vol *Volume, rec dirRecord) ([]byte, error) {
	if rec.isDir() {
		return nil, ErrNotRegular
	}
	if rec.Flags&flagMultiExt != 0 {
		return nil, fmt.Errorf("%w: multi-extent files not supported", ErrCorrupt)
	}
	data := make([]byte, rec.Size)
	if rec.Size == 0 {
		return data, nil
	}
	if _, err := rs.ReadAt(data, int64(rec.ExtentLBA)*int64(vol.BlockSize)); err != nil {
		return nil, fmt.Errorf("iso9660: read file extent @LBA %d: %w", rec.ExtentLBA, err)
	}
	return data, nil
}
