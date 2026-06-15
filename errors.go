// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

// Package iso9660 is a pure-Go, read-only driver for the ISO 9660 (ECMA-119)
// CD/DVD image format produced by mkisofs/genisoimage/xorriso. It reads the
// Primary Volume Descriptor, walks directory records, and reads contiguous
// file extents. ISO 9660 is a read-only format; every mutating method of
// filesystem.Filesystem returns ErrReadOnly.
//
// The base ECMA-119 layer (uppercase ;version-suffixed names) plus the Rock
// Ridge (POSIX long names/permissions/symlinks, including CE continuation
// areas) and Joliet (UCS-2 long names) extensions are decoded. When a tree
// carries Rock Ridge it takes precedence, then Joliet, then the base tree.
package iso9660

import "errors"

// Sentinel errors. Compare with errors.Is so wrapped errors continue to match.
var (
	// ErrReadOnly is returned by every mutating method (WriteFile, MkDir,
	// DeleteFile, DeleteDir, Rename). ISO 9660 is a read-only format.
	ErrReadOnly = errors.New("iso9660: filesystem is read-only")

	// ErrBadDescriptor is returned when no valid Primary Volume Descriptor
	// (standard identifier "CD001") is found.
	ErrBadDescriptor = errors.New("iso9660: no valid primary volume descriptor")

	// ErrNotFound is returned when a path component cannot be located.
	ErrNotFound = errors.New("iso9660: path not found")

	// ErrNotDirectory is returned when ListDir targets a non-directory.
	ErrNotDirectory = errors.New("iso9660: not a directory")

	// ErrNotRegular is returned when ReadFile targets a non-regular file.
	ErrNotRegular = errors.New("iso9660: not a regular file")

	// ErrNotSymlink is returned by ReadLink when the target is not a symlink.
	// Symlinks come from the Rock Ridge SL entry (base ISO 9660 has none), so
	// this is also what ReadLink returns on a plain (non-Rock-Ridge) image.
	ErrNotSymlink = errors.New("iso9660: not a symbolic link")

	// ErrTooManyLinks is returned when path resolution exceeds the symlink hop
	// limit, indicating a loop.
	ErrTooManyLinks = errors.New("iso9660: too many symbolic link traversals")

	// ErrCorrupt is returned when an on-disk structure fails a sanity check.
	ErrCorrupt = errors.New("iso9660: corrupt image")
)
