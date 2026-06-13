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
	rs       io.ReaderAt
	size     int64
	vol      *Volume
	suspSkip int // SUSP LEN_SKP: bytes to skip at the start of each System Use Area
	closer   io.Closer
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
	fs.suspSkip = fs.detectSUSPSkip()
	return fs, nil
}

// detectSUSPSkip reads the root "." entry and returns the SUSP LEN_SKP (0 when
// the image carries no SP entry / no Rock Ridge).
func (fs *FS) detectSUSPSkip() int {
	entries, err := readDirRecords(fs.rs, fs.vol, fs.vol.Root)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if len(e.rawName) == 1 && e.rawName[0] == 0x00 { // "." entry
			return detectSUSPSkip(e.sysUse)
		}
	}
	return 0
}

// rrFor parses the Rock Ridge attributes of a record (past the SUSP skip).
func (fs *FS) rrFor(rec dirRecord) rrInfo {
	if len(rec.sysUse) <= fs.suspSkip {
		return rrInfo{}
	}
	return parseRockRidge(rec.sysUse[fs.suspSkip:])
}

// effectiveName is the Rock Ridge name when present, else the ISO 9660 name.
func (fs *FS) effectiveName(rec dirRecord) string {
	if rr := fs.rrFor(rec); rr.hasName {
		return rr.name
	}
	return rec.Name
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
		// Prefer an exact match on the effective (Rock Ridge or ISO) name;
		// fall back to case-insensitive, since base ISO names are uppercased.
		found := false
		for _, e := range entries {
			if !e.isSpecial() && fs.effectiveName(e) == name {
				cur, found = e, true
				break
			}
		}
		if !found {
			for _, e := range entries {
				if !e.isSpecial() && strings.EqualFold(fs.effectiveName(e), name) {
					cur, found = e, true
					break
				}
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
		out = append(out, filesystem.NewDirEntry(uint64(e.ExtentLBA), fs.effectiveName(e), ftype))
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
	// Prefer the Rock Ridge POSIX mode; otherwise synthesise from the type.
	if rr := fs.rrFor(rec); rr.hasMode {
		return filesystem.NewStat(rr.mode, uint64(rec.Size), uint64(rec.ExtentLBA)), nil
	}
	mode := uint16(modeFileDefault)
	if rec.isDir() {
		mode = modeDirDefault
	}
	return filesystem.NewStat(mode, uint64(rec.Size), uint64(rec.ExtentLBA)), nil
}

// ReadLink returns the target of a Rock Ridge symbolic link. Base ISO 9660
// without Rock Ridge has no symlinks, so ReadLink reports ErrNotSymlink.
func (fs *FS) ReadLink(path string) (string, error) {
	rec, err := fs.resolve(path)
	if err != nil {
		return "", err
	}
	if rr := fs.rrFor(rec); rr.isSymlink {
		return rr.symlink, nil
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
