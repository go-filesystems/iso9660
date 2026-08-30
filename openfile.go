// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import (
	"fmt"
	"io"
	"os"
	"sort"
	"sync/atomic"

	filesystem "github.com/go-filesystems/interface"
)

// Verify implementation of the optional read-at-an-offset interface.
var _ filesystem.Opener = (*FS)(nil)

// isoFile is an open regular file on an ISO 9660 volume, backing
// filesystem.File.
//
// ISO 9660 is the easy case for random access and the reason this driver gets
// the capability first: file data is stored in contiguous extents whose
// location is already in the directory record. A base file is one extent; a
// multi-extent file (ECMA-119 §6.5.1) is the in-order concatenation of several,
// and the sizes are byte lengths, not block counts, so extent boundaries do NOT
// fall on logical-block boundaries. Mapping a file offset to a disk offset is
// therefore a search over the extent list, never a division.
//
// The extent list is built once at OpenFile from metadata the driver has
// already parsed; no file data is read. Every field is written there and only
// read afterwards, so concurrent ReadAt calls need no synchronisation of their
// own, as io.ReaderAt requires. The one mutable field, closed, is atomic.
type isoFile struct {
	rs io.ReaderAt
	// extents are in file order and contiguous in fileOff: extents[i] covers
	// file bytes [fileOff, fileOff+size). Zero-length extents are dropped, as
	// readFile drops them.
	extents []fileExtent
	// size is the record's data length. covered is what the extents actually
	// address; a record whose extents fall short reads as zeros past covered,
	// which is exactly what readFile produces (it allocates size and fills
	// only what the extents cover).
	size    int64
	covered int64
	closed  atomic.Bool
}

// fileExtent is one extent placed in the file's byte space: where it starts in
// the file, where it starts on disk, and how long it is.
type fileExtent struct {
	fileOff int64
	diskOff int64
	size    int64
}

var _ filesystem.File = (*isoFile)(nil)

// OpenFile opens the regular file at path for random access.
//
// It resolves the path exactly as ReadFile does — following Rock Ridge
// symlinks — and then records where the file's extents live, without reading
// any of them. Note that this METHOD on *FS is distinct from the package-level
// OpenFile(path) function, which opens an image file from the host filesystem
// and returns an *FS; Go keeps package scope and method sets apart, and the
// method is the one filesystem.Opener asks for.
func (fs *FS) OpenFile(path string) (filesystem.File, error) {
	rec, err := fs.resolve(path)
	if err != nil {
		return nil, err
	}
	return fs.openRecord(rec)
}

// openRecord turns a resolved directory record into an isoFile. Split out from
// OpenFile so the extent-list construction can be exercised directly against a
// hand-built record, the way the multi-extent tests do.
func (fs *FS) openRecord(rec dirRecord) (filesystem.File, error) {
	if rec.isDir() {
		return nil, ErrNotRegular
	}
	// A data length larger than the whole image is corruption, not a big
	// file: reject it the way ReadFile does. The check is deliberately NOT
	// the allocCeiling used for reads — that ceiling exists to bound an
	// ALLOCATION, and this path allocates nothing, so applying its 1 GiB
	// unknown-size fallback here would refuse legitimately large files for
	// no reason. When the image size is unknown there is simply nothing to
	// compare against, and a bogus extent surfaces as a read error instead.
	if fs.size >= 0 && int64(rec.Size) > fs.size {
		return nil, fmt.Errorf("%w: file size %d exceeds image size %d", ErrCorrupt, rec.Size, fs.size)
	}
	exts := rec.extents
	if exts == nil {
		exts = []extent{{lba: rec.ExtentLBA, size: rec.Size}}
	}
	size := int64(rec.Size)
	placed := make([]fileExtent, 0, len(exts))
	var covered int64
	for _, e := range exts {
		if e.size == 0 {
			continue
		}
		// Defend against a forged extent list whose sizes overrun the
		// record's Size — readFile catches this with safeio.Slice before it
		// can panic on a slice bound, and this path must agree with it
		// rather than silently expose bytes past the declared end.
		if covered+int64(e.size) > size {
			return nil, fmt.Errorf("%w: file extent @LBA %d overruns size %d", ErrCorrupt, e.lba, size)
		}
		placed = append(placed, fileExtent{
			fileOff: covered,
			diskOff: int64(e.lba) * int64(fs.vol.BlockSize),
			size:    int64(e.size),
		})
		covered += int64(e.size)
	}
	return &isoFile{rs: fs.rs, extents: placed, size: size, covered: covered}, nil
}

// Size returns the file's data length in bytes, taken from the directory
// record parsed at OpenFile time. No I/O.
func (f *isoFile) Size() int64 { return f.size }

// Close releases the File. ISO 9660 files hold no per-file handle — the image
// handle stays owned by the FS — so Close only marks the File unusable, which
// turns a use-after-close into a clear os.ErrClosed instead of a silent read
// through a stale extent list. It is idempotent.
func (f *isoFile) Close() error {
	f.closed.Store(true)
	return nil
}

// ReadAt implements io.ReaderAt to the letter, the contract io.SectionReader
// and every generic consumer silently depend on:
//
//   - p is filled completely with a nil error whenever the bytes exist;
//   - n < len(p) comes back only together with a non-nil error;
//   - a read running into the end of the file returns io.EOF with whatever
//     bytes it did get, and an offset at or past Size() returns 0, io.EOF.
//
// Each iteration locates the extent holding the current file offset by binary
// search — extent boundaries are byte lengths and do not divide evenly — and
// issues one read per extent crossed, never more than the caller asked for.
// Bytes past what the extents cover are zeros, matching what ReadFile returns
// for the same record.
func (f *isoFile) ReadAt(p []byte, off int64) (int, error) {
	if f.closed.Load() {
		return 0, os.ErrClosed
	}
	if off < 0 {
		return 0, fmt.Errorf("iso9660: ReadAt: negative offset %d", off)
	}
	if off >= f.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) {
		cur := off + int64(n)
		if cur >= f.size {
			return n, io.EOF
		}
		want := int64(len(p) - n)
		if rem := f.size - cur; want > rem {
			want = rem
		}
		if cur >= f.covered {
			// Tail the extents do not reach: readFile leaves these bytes
			// zero, so they read as zero here too.
			for i := int64(0); i < want; i++ {
				p[n+int(i)] = 0
			}
			n += int(want)
			continue
		}
		i := sort.Search(len(f.extents), func(i int) bool {
			return f.extents[i].fileOff+f.extents[i].size > cur
		})
		e := f.extents[i]
		chunk := e.fileOff + e.size - cur
		if chunk > want {
			chunk = want
		}
		m, err := f.rs.ReadAt(p[n:n+int(chunk)], e.diskOff+(cur-e.fileOff))
		n += m
		if err != nil {
			return n, fmt.Errorf("iso9660: read file extent @offset %d: %w", e.diskOff, err)
		}
	}
	return n, nil
}
