// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import (
	"fmt"
	"io"
	"os"
	"strings"

	filesystem "github.com/go-filesystems/interface"
)

// Unix mode type bits used to synthesise a Stat mode (base ISO 9660 carries no
// POSIX permissions; those would come from Rock Ridge).
const (
	sIFDIR = 0x4000
	sIFREG = 0x8000
	modeDirDefault = sIFDIR | 0o555
	modeFileDefault = sIFREG | 0o444
)

// FS is an opened, read-only ISO 9660 filesystem.
type FS struct {
	rs     io.ReaderAt
	size   int64
	vol    *Volume
	closer io.Closer
}

var _ filesystem.Filesystem = (*FS)(nil)

// Open parses the ISO 9660 volume descriptors and returns a read-only handle.
// The caller retains ownership of rs unless it implements io.Closer. Pass
// size = -1 if unknown.
func Open(rs io.ReaderAt, size int64) (*FS, error) {
	vol, err := readVolume(rs)
	if err != nil {
		return nil, err
	}
	fs := &FS{rs: rs, size: size, vol: vol}
	if c, ok := rs.(io.Closer); ok {
		fs.closer = c
	}
	return fs, nil
}

// OpenFile opens path read-only and wires it into Open.
func OpenFile(path string) (*FS, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("iso9660: open %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("iso9660: stat %s: %w", path, err)
	}
	fs, err := Open(f, st.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	return fs, nil
}

// Volume returns the decoded Primary Volume Descriptor. Owned by FS.
func (fs *FS) Volume() *Volume { return fs.vol }

// Close releases the backing file handle if FS opened one.
func (fs *FS) Close() error {
	if fs.closer != nil {
		return fs.closer.Close()
	}
	return nil
}

// resolve walks an absolute path from the root directory record. Component
// matching is case-insensitive because base ISO 9660 stores names uppercased.
func (fs *FS) resolve(path string) (dirRecord, error) {
	cur := fs.vol.Root
	for _, name := range splitPath(path) {
		if !cur.isDir() {
			return dirRecord{}, fmt.Errorf("%w: %q", ErrNotDirectory, name)
		}
		entries, err := readDirRecords(fs.rs, fs.vol, cur)
		if err != nil {
			return dirRecord{}, err
		}
		found := false
		for _, e := range entries {
			if e.isSpecial() {
				continue
			}
			if strings.EqualFold(e.Name, name) {
				cur = e
				found = true
				break
			}
		}
		if !found {
			return dirRecord{}, fmt.Errorf("%w: %q", ErrNotFound, name)
		}
	}
	return cur, nil
}

// ReadFile returns the full contents of the regular file at path.
func (fs *FS) ReadFile(path string) ([]byte, error) {
	rec, err := fs.resolve(path)
	if err != nil {
		return nil, err
	}
	return readFile(fs.rs, fs.vol, rec)
}

// ListDir enumerates the directory at path, excluding the "." and ".."
// special entries.
func (fs *FS) ListDir(path string) ([]filesystem.DirEntry, error) {
	rec, err := fs.resolve(path)
	if err != nil {
		return nil, err
	}
	entries, err := readDirRecords(fs.rs, fs.vol, rec)
	if err != nil {
		return nil, err
	}
	out := make([]filesystem.DirEntry, 0, len(entries))
	for _, e := range entries {
		if e.isSpecial() {
			continue
		}
		ftype := uint8(0)
		if e.isDir() {
			ftype = 2 // directory
		}
		out = append(out, filesystem.NewDirEntry(uint64(e.ExtentLBA), e.Name, ftype))
	}
	return out, nil
}

// Stat resolves path and returns a synthesised mode (type + default perms),
// size and a pseudo-inode (the extent LBA).
func (fs *FS) Stat(path string) (filesystem.Stat, error) {
	rec, err := fs.resolve(path)
	if err != nil {
		return nil, err
	}
	mode := uint16(modeFileDefault)
	if rec.isDir() {
		mode = modeDirDefault
	}
	return filesystem.NewStat(mode, uint64(rec.Size), uint64(rec.ExtentLBA)), nil
}

// ReadLink always fails on base ISO 9660: symlinks require the Rock Ridge
// extension, which is not yet decoded.
func (fs *FS) ReadLink(path string) (string, error) {
	if _, err := fs.resolve(path); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%w: %s", ErrNotSymlink, path)
}

// --- Mutating methods: ISO 9660 is a read-only format. ---

func (fs *FS) WriteFile(string, []byte, os.FileMode) error { return ErrReadOnly }
func (fs *FS) MkDir(string, os.FileMode) error             { return ErrReadOnly }
func (fs *FS) DeleteFile(string) error                     { return ErrReadOnly }
func (fs *FS) DeleteDir(string) error                      { return ErrReadOnly }
func (fs *FS) Rename(string, string) error                 { return ErrReadOnly }

// splitPath normalises an absolute path into its non-empty, non-"." components.
func splitPath(p string) []string {
	out := make([]string, 0, 8)
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." {
			continue
		}
		out = append(out, s)
	}
	return out
}
