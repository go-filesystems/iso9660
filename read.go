// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import (
	"bytes"
	"fmt"
	"io"
	"slices"

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

// maxDirExtent bounds one directory's extent, 16 MiB. A directory's size is a
// u32 on disk, but its records are at least 34 bytes each and parse into
// several times that (a dirRecord plus its name and System Use copies), so a
// 4 GiB claim over a large real image would otherwise cost most of the image
// again, several times over, on every lookup that passes through it.
const maxDirExtent = 16 << 20

// growChunk is the first step of a read whose size could not be checked against
// an image size: the buffer then grows (doubling) with the bytes the reader
// actually delivers instead of being reserved from the claim.
const growChunk = 1 << 20

// maxInt is the largest slice length of the target: 2^31-1 on a 32-bit build,
// where a u32 length from the image can otherwise convert to a negative int.
const maxInt = int64(^uint(0) >> 1)

// checkSize reports safeio.ErrTooLarge when n is negative, exceeds the
// allocation ceiling, or cannot be a slice length on this target.
func checkSize(n, maxAlloc int64) error {
	if n < 0 || maxAlloc < 0 || n > maxAlloc || n > maxInt {
		return fmt.Errorf("%w: size %d exceeds ceiling %d", safeio.ErrTooLarge, n, min(maxAlloc, maxInt))
	}
	return nil
}

// sizeUnknown reports whether maxAlloc is the fallback for an unknown image
// size rather than the image size itself.
func sizeUnknown(maxAlloc int64) bool { return maxAlloc == maxUnknownAlloc }

// checkExtent reports ErrOutOfBounds when [off, off+n) is not inside an image
// of maxAlloc bytes. With the size unknown there is nothing to compare against;
// the read itself reports a short image.
func checkExtent(off, n, maxAlloc int64) error {
	if sizeUnknown(maxAlloc) {
		return nil
	}
	if off < 0 || n < 0 || off > maxAlloc-n {
		return fmt.Errorf("%w: extent [%d,+%d) outside the %d-byte image", safeio.ErrOutOfBounds, off, n, maxAlloc)
	}
	return nil
}

// appendAt appends the n bytes at off of rs to dst and returns the extended
// slice. When the image size is known the caller has already checked that the
// bytes exist, so the room is reserved at once; when it is not (maxAlloc is the
// unknown-size fallback) the buffer grows with what rs delivers, so a forged
// 1 GiB length over a 40 KiB image costs 40 KiB, not 1 GiB.
func appendAt(rs io.ReaderAt, dst []byte, off, n, maxAlloc int64) ([]byte, int64, error) {
	grow := sizeUnknown(maxAlloc)
	done := int64(0)
	for done < n {
		step := n - done
		if grow && int64(cap(dst)-len(dst)) < step {
			step = min(step, max(growChunk, int64(len(dst))))
		}
		old := len(dst)
		dst = slices.Grow(dst, int(step))[:old+int(step)]
		m, err := rs.ReadAt(dst[old:], off+done)
		if err != nil && !(err == io.EOF && int64(m) == step) {
			return dst[:old+m], done + int64(m), err
		}
		done += step
	}
	return dst, done, nil
}

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
	if rec.Size > maxDirExtent {
		return nil, fmt.Errorf("%w: dir extent size %d: %w (directory limit %d)",
			ErrCorrupt, rec.Size, safeio.ErrTooLarge, maxDirExtent)
	}
	if err := checkSize(int64(rec.Size), maxAlloc); err != nil {
		return nil, fmt.Errorf("%w: dir extent size %d: %w", ErrCorrupt, rec.Size, err)
	}
	if err := checkExtent(int64(rec.ExtentLBA)*int64(bs), int64(rec.Size), maxAlloc); err != nil {
		return nil, fmt.Errorf("%w: dir extent @LBA %d: %w", ErrCorrupt, rec.ExtentLBA, err)
	}
	data, _, err := appendAt(rs, nil, int64(rec.ExtentLBA)*int64(bs), int64(rec.Size), maxAlloc)
	if err != nil {
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
	total := int64(rec.Size)
	if err := checkSize(total, maxAlloc); err != nil {
		return nil, fmt.Errorf("%w: file size %d: %w", ErrCorrupt, rec.Size, err)
	}
	// Validate every extent against the declared size and, when the image size
	// is known, against the image BEFORE reserving the output: a record whose
	// extents lie outside the image costs nothing. With the size unknown the
	// output grows with the bytes the reader delivers (see appendAt) rather
	// than being reserved from the claim.
	var covered int64
	for _, e := range exts {
		if e.size == 0 {
			continue
		}
		// Defend against a forged extent list whose sizes overrun the merged
		// Size.
		if covered+int64(e.size) > total {
			return nil, fmt.Errorf("%w: file extent @LBA %d: %w", ErrCorrupt, e.lba,
				fmt.Errorf("%w: range [%d,%d+%d) exceeds buffer length %d",
					safeio.ErrOutOfBounds, covered, covered, e.size, total))
		}
		if err := checkExtent(int64(e.lba)*int64(vol.BlockSize), int64(e.size), maxAlloc); err != nil {
			return nil, fmt.Errorf("%w: file extent @LBA %d: %w", ErrCorrupt, e.lba, err)
		}
		covered += int64(e.size)
	}
	if covered < total && sizeUnknown(maxAlloc) {
		// Zero-filling the uncovered tail would reserve the claim after all.
		return nil, fmt.Errorf("%w: extents cover %d of %d bytes", ErrCorrupt, covered, total)
	}
	var data []byte
	if !sizeUnknown(maxAlloc) {
		data = make([]byte, 0, total)
	}
	for _, e := range exts {
		if e.size == 0 {
			continue
		}
		var err error
		data, _, err = appendAt(rs, data, int64(e.lba)*int64(vol.BlockSize), int64(e.size), maxAlloc)
		if err != nil {
			return nil, fmt.Errorf("iso9660: read file extent @LBA %d: %w", e.lba, err)
		}
	}
	// Bytes past what the extents cover read as zeros (known size only: the
	// capacity is already there).
	if data == nil {
		data = []byte{}
	}
	return data[:total:total], nil
}
