// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import (
	"bytes"
	"fmt"
	"io"

	"github.com/go-volumes/safeio"
)

// allocCeiling returns the upper bound for a single allocation derived from an
// attacker-controlled on-disk length field. When the backing image size is
// known (size >= 0) that is the natural ceiling: no structure can be larger
// than the whole image. When it is unknown (Open called with size = -1) we
// fall back to a generous but finite cap so a forged 4 GiB length still fails
// gracefully instead of OOM-ing the host.
func allocCeiling(size int64) int64 {
	if size >= 0 {
		return size
	}
	return maxUnknownAlloc
}

// maxUnknownAlloc bounds a single allocation when the image size is unknown.
// 1 GiB is far beyond any legitimate ISO 9660 directory extent or single file
// extent (a base file extent length is a u32, capped at 4 GiB) yet still small
// enough that a forged length cannot exhaust host memory.
const maxUnknownAlloc = 1 << 30

// readDirRecords reads and parses the directory whose extent is described by
// rec. ISO 9660 directory records never span a logical sector, so a zero
// length byte means "skip to the next sector". The "." and ".." entries are
// included; callers filter them as needed. maxAlloc bounds the directory
// extent allocation against a malicious Size field (typically the image size).
func readDirRecords(rs io.ReaderAt, vol *Volume, rec dirRecord, maxAlloc int64) ([]dirRecord, error) {
	if !rec.isDir() {
		return nil, ErrNotDirectory
	}
	bs := int(vol.BlockSize)
	data, err := safeio.MakeBytes(int64(rec.Size), maxAlloc)
	if err != nil {
		return nil, fmt.Errorf("%w: dir extent size %d: %w", ErrCorrupt, rec.Size, err)
	}
	if _, err := rs.ReadAt(data, int64(rec.ExtentLBA)*int64(bs)); err != nil {
		return nil, fmt.Errorf("iso9660: read dir extent @LBA %d: %w", rec.ExtentLBA, err)
	}

	var raw []dirRecord
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
		raw = append(raw, child)
		pos += n
	}
	return mergeMultiExtent(raw)
}

// mergeMultiExtent collapses runs of consecutive directory records that make up
// a single multi-extent file (ECMA-119 §6.5.1) into one record. Such a file is
// recorded as several consecutive records sharing the same file identifier
// where every record but the last has the multi-extent flag (flagMultiExt)
// set; the file content is the concatenation of each record's extent. The
// merged record keeps the first record's identity (name, System Use Area) but
// gains an explicit extent list and a total Size. Records that are not part of
// such a run pass through unchanged. A run that ends without a final
// flag-clear record (truncated/corrupt) is rejected with ErrCorrupt.
func mergeMultiExtent(in []dirRecord) ([]dirRecord, error) {
	out := make([]dirRecord, 0, len(in))
	for i := 0; i < len(in); i++ {
		rec := in[i]
		// A directory or a record without the multi-extent flag is a complete
		// entry on its own.
		if rec.isDir() || rec.Flags&flagMultiExt == 0 {
			out = append(out, rec)
			continue
		}
		// Start of a multi-extent run: gather following records that share the
		// same raw file identifier until one without the flag terminates it.
		merged := rec
		merged.extents = []extent{{lba: rec.ExtentLBA, size: rec.Size}}
		total := uint64(rec.Size)
		j := i + 1
		complete := false
		for ; j < len(in); j++ {
			next := in[j]
			if !bytes.Equal(next.rawName, rec.rawName) {
				break
			}
			merged.extents = append(merged.extents, extent{lba: next.ExtentLBA, size: next.Size})
			total += uint64(next.Size)
			if next.Flags&flagMultiExt == 0 {
				// Final extent: clear the multi-extent flag on the merged record.
				merged.Flags = next.Flags &^ flagMultiExt
				complete = true
				j++
				break
			}
		}
		if !complete {
			return nil, fmt.Errorf("%w: multi-extent file %q has no final extent", ErrCorrupt, cleanName(rec.rawName))
		}
		if total > uint64(^uint32(0)) {
			return nil, fmt.Errorf("%w: multi-extent file %q exceeds 4 GiB", ErrCorrupt, cleanName(rec.rawName))
		}
		merged.Size = uint32(total)
		out = append(out, merged)
		i = j - 1
	}
	return out, nil
}

// readFile returns the full contents of the file described by rec. A base
// ISO 9660 file occupies a single contiguous extent; a multi-extent file
// (ECMA-119 §6.5.1, recorded as a run of consecutive records merged by
// mergeMultiExtent) is the in-order concatenation of its extents. maxAlloc
// bounds the result allocation against a malicious Size field (typically the
// image size).
func readFile(rs io.ReaderAt, vol *Volume, rec dirRecord, maxAlloc int64) ([]byte, error) {
	if rec.isDir() {
		return nil, ErrNotRegular
	}
	exts := rec.extents
	if exts == nil {
		exts = []extent{{lba: rec.ExtentLBA, size: rec.Size}}
	}
	data, err := safeio.MakeBytes(int64(rec.Size), maxAlloc)
	if err != nil {
		return nil, fmt.Errorf("%w: file size %d: %w", ErrCorrupt, rec.Size, err)
	}
	off := 0
	for _, e := range exts {
		if e.size == 0 {
			continue
		}
		// Defend against a forged extent list whose sizes overrun the merged
		// Size (a slice-bounds panic otherwise): validate before slicing.
		dst, err := safeio.Slice(data, off, int(e.size))
		if err != nil {
			return nil, fmt.Errorf("%w: file extent @LBA %d: %w", ErrCorrupt, e.lba, err)
		}
		if _, err := rs.ReadAt(dst, int64(e.lba)*int64(vol.BlockSize)); err != nil {
			return nil, fmt.Errorf("iso9660: read file extent @LBA %d: %w", e.lba, err)
		}
		off += int(e.size)
	}
	return data, nil
}
