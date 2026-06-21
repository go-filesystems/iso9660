// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// This file implements writing (mastering) a base ECMA-119 ISO 9660 image.
// It covers the same layer the reader decodes: the system area, a Primary
// Volume Descriptor, a Volume Descriptor Set Terminator, the root and
// subdirectory extents (with "." and ".." records), the Type-L and Type-M
// path tables and the file data extents. Identifiers are written at
// interchange level 1: uppercase, with a ";1" version suffix on files.
//
// Rock Ridge (POSIX names/perms/symlinks) and Joliet (UCS-2 long names) are
// NOT written; they remain follow-ups. Images produced here therefore carry
// only the uppercase ";version"-suffixed names of the base standard.

// dirRecordHeaderLen is the fixed prefix of a directory record before the
// variable-length identifier (ECMA-119 9.1).
const dirRecordHeaderLen = 33

// node is an entry in the in-memory tree being mastered. A node is either a
// directory (children != nil semantics via isDir) or a regular file (data).
type node struct {
	name     string // caller-supplied component name (mixed case allowed)
	isDir    bool
	data     []byte           // file contents (files only)
	children map[string]*node // child nodes by caller name (dirs only)

	// Assigned during layout.
	id        string // on-disk identifier (uppercase; ";1" on files)
	extentLBA uint32 // first logical block of this node's extent
	extentLen uint32 // extent length in bytes (dir record area or file size)
	parent    *node
	dirIndex  int // 1-based path-table index (dirs only); root is 1
}

// Builder accumulates a directory tree in memory and then writes it as a base
// ISO 9660 image. Construct one with NewBuilder, populate it with AddDir and
// AddFile, then call WriteTo.
//
// Builder is the write-side counterpart to Open: an image produced here reads
// back through this package's own reader (see the round-trip tests).
type Builder struct {
	root     *node
	volumeID string
	created  time.Time

	pathTableMLBA uint32 // set in layout, after the Type-L table size is known
}

// pathTableLLBA is the fixed Type-L path-table location: the system area is
// sectors 0..15, the PVD is 16 and the terminator is 17, so the Type-L path
// table begins at sector 18. The Type-M table, directories and file data
// follow at offsets computed in layout.
const pathTableLLBA uint32 = 18

// NewBuilder returns an empty Builder whose root directory is "/". volumeID
// becomes the Primary Volume Descriptor volume identifier (uppercased and
// truncated to the 32-byte ECMA-119 field; empty yields "CDROM").
func NewBuilder(volumeID string) *Builder {
	return &Builder{
		root:     &node{name: "/", isDir: true, children: map[string]*node{}},
		volumeID: volumeID,
		created:  time.Now(),
	}
}

// AddDir creates the directory at the slash-separated absolute path, creating
// any missing parents. It is idempotent for an already-existing directory and
// returns an error if a path component already exists as a regular file.
func (b *Builder) AddDir(path string) error {
	_, err := b.mkdirAll(splitPath(path))
	return err
}

// AddFile creates the regular file at the slash-separated absolute path with
// the given contents, creating any missing parent directories. An existing
// file at the path is replaced; an existing directory there is an error.
func (b *Builder) AddFile(path string, data []byte) error {
	parts := splitPath(path)
	if len(parts) == 0 {
		return fmt.Errorf("%w: empty file path", ErrInvalidName)
	}
	dir, err := b.mkdirAll(parts[:len(parts)-1])
	if err != nil {
		return err
	}
	name := parts[len(parts)-1]
	if existing, ok := dir.children[name]; ok && existing.isDir {
		return fmt.Errorf("%w: %q is a directory", ErrExists, name)
	}
	dir.children[name] = &node{name: name, data: append([]byte(nil), data...)}
	return nil
}

// mkdirAll walks/creates the directory chain named by parts under the root.
func (b *Builder) mkdirAll(parts []string) (*node, error) {
	cur := b.root
	for _, name := range parts {
		child, ok := cur.children[name]
		if !ok {
			child = &node{name: name, isDir: true, children: map[string]*node{}}
			cur.children[name] = child
			cur = child
			continue
		}
		if !child.isDir {
			return nil, fmt.Errorf("%w: %q is a file", ErrExists, name)
		}
		cur = child
	}
	return cur, nil
}

// WriteTo masters the accumulated tree into a base ISO 9660 image written
// through w. w is written sector by sector at absolute offsets, so a freshly
// created (or truncated) file or an in-memory buffer is the expected target.
func (b *Builder) WriteTo(w io.WriterAt) error {
	dirs := b.layout()

	// 1. System area: 16 zeroed logical sectors.
	for s := int64(0); s < systemAreaSectors; s++ {
		if err := writeSector(w, s, nil); err != nil {
			return err
		}
	}

	// 2. Primary Volume Descriptor and 3. Set Terminator.
	if err := writeSector(w, 16, b.primaryVolumeDescriptor(dirs)); err != nil {
		return err
	}
	if err := writeSector(w, 17, volumeDescriptorTerminator()); err != nil {
		return err
	}

	// 4. Path tables (Type-L then Type-M), each padded to whole sectors.
	if err := writeAt(w, int64(pathTableLLBA)*sectorSize, b.pathTable(dirs, false)); err != nil {
		return err
	}
	if err := writeAt(w, int64(b.pathTableMLBA)*sectorSize, b.pathTable(dirs, true)); err != nil {
		return err
	}

	// 5. Directory extents (root first, breadth-first).
	for _, d := range dirs {
		if err := writeAt(w, int64(d.extentLBA)*sectorSize, b.directoryExtent(d)); err != nil {
			return err
		}
	}

	// 6. File data extents.
	var writeFiles func(d *node) error
	writeFiles = func(d *node) error {
		for _, c := range sortedChildren(d) {
			if c.isDir {
				if err := writeFiles(c); err != nil {
					return err
				}
				continue
			}
			if len(c.data) == 0 {
				continue
			}
			if err := writeAt(w, int64(c.extentLBA)*sectorSize, c.data); err != nil {
				return err
			}
		}
		return nil
	}
	return writeFiles(b.root)
}

// layout assigns identifiers, extent LBAs and sizes to every node and returns
// the directories in path-table order (root first, then by parent index then
// identifier). It must run before any descriptor or extent is serialised.
func (b *Builder) layout() []*node {
	// Assign on-disk identifiers and wire up parent pointers.
	assignIdentifiers(b.root, nil)

	// Collect directories breadth-first; number them for the path table.
	dirs := orderedDirs(b.root)
	for i, d := range dirs {
		d.dirIndex = i + 1
	}

	// Path-table size determines where the Type-M table and the first
	// directory extent land.
	ptBytes := b.pathTableSize(dirs)
	ptSectors := sectorsFor(uint32(ptBytes))
	b.pathTableMLBA = pathTableLLBA + ptSectors
	next := b.pathTableMLBA + ptSectors

	// Directory extents, in path-table order.
	for _, d := range dirs {
		d.extentLen = b.directoryExtentSize(d)
		d.extentLBA = next
		next += sectorsFor(d.extentLen)
	}

	// File data extents, in the same traversal order WriteTo uses.
	var place func(d *node)
	place = func(d *node) {
		for _, c := range sortedChildren(d) {
			if c.isDir {
				place(c)
				continue
			}
			c.extentLen = uint32(len(c.data))
			// Empty files get a single (notional) extent at the current
			// position but occupy no sectors.
			c.extentLBA = next
			next += sectorsFor(c.extentLen)
		}
	}
	place(b.root)

	return dirs
}

// assignIdentifiers sets parent pointers and the on-disk identifier of every
// node: directories get an uppercase, level-1-truncated name; files get the
// uppercase 8.3 name plus a ";1" version suffix.
func assignIdentifiers(n *node, parent *node) {
	n.parent = parent
	if n.parent == nil {
		n.id = "" // root identifier is the single byte 0x00, handled inline
	}
	// Resolve sibling identifier collisions deterministically.
	used := map[string]bool{}
	for _, c := range sortedChildren(n) {
		base := isoIdentifier(c.name, c.isDir)
		id := base
		for k := 2; used[id]; k++ {
			id = disambiguate(base, c.isDir, k)
		}
		used[id] = true
		c.id = id
		assignIdentifiers(c, n)
	}
}

// orderedDirs returns every directory node in path-table order: root first,
// then in order of increasing parent path-table index, ties broken by
// identifier (ECMA-119 6.9.1).
func orderedDirs(root *node) []*node {
	dirs := []*node{root}
	for i := 0; i < len(dirs); i++ {
		for _, c := range sortedChildren(dirs[i]) {
			if c.isDir {
				dirs = append(dirs, c)
			}
		}
	}
	return dirs
}

// sortedChildren returns a directory's children sorted by on-disk identifier
// (when assigned) else by caller name, for stable, standard-ordered output.
func sortedChildren(d *node) []*node {
	if !d.isDir {
		return nil
	}
	out := make([]*node, 0, len(d.children))
	for _, c := range d.children {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].id, out[j].id
		if a == "" || b == "" {
			a, b = out[i].name, out[j].name
		}
		return a < b
	})
	return out
}

// directoryExtentSize returns the byte length of a directory's record area,
// rounding each record across sector boundaries the way directoryExtent emits
// them (a record never spans a logical sector, ECMA-119 6.8.1.1).
func (b *Builder) directoryExtentSize(d *node) uint32 {
	pos := 0
	add := func(idLen int) {
		rl := dirRecordLen(idLen)
		if pos/sectorSize != (pos+rl-1)/sectorSize {
			pos = (pos/sectorSize + 1) * sectorSize // pad to next sector
		}
		pos += rl
	}
	add(1) // "."
	add(1) // ".."
	for _, c := range sortedChildren(d) {
		add(len(c.id))
	}
	return uint32((pos + sectorSize - 1) / sectorSize * sectorSize)
}

// directoryExtent serialises a directory's records: "." then ".." then each
// child by identifier. A record is never split across a sector; the trailing
// bytes of a sector are zero-padded.
func (b *Builder) directoryExtent(d *node) []byte {
	buf := make([]byte, d.extentLen)
	pos := 0
	emit := func(rec []byte) {
		if pos/sectorSize != (pos+len(rec)-1)/sectorSize {
			pos = (pos/sectorSize + 1) * sectorSize
		}
		copy(buf[pos:], rec)
		pos += len(rec)
	}
	parent := d.parent
	if parent == nil {
		parent = d // root's ".." points at itself
	}
	emit(b.dirRecord([]byte{0x00}, d.extentLBA, d.extentLen, true))
	emit(b.dirRecord([]byte{0x01}, parent.extentLBA, parent.extentLen, true))
	for _, c := range sortedChildren(d) {
		emit(b.dirRecord([]byte(c.id), c.extentLBA, c.extentLen, c.isDir))
	}
	return buf
}

// dirRecord builds one directory record (ECMA-119 9.1). flagDir sets the
// directory file-flag bit.
func (b *Builder) dirRecord(id []byte, extentLBA, size uint32, isDir bool) []byte {
	rl := dirRecordLen(len(id))
	rec := make([]byte, rl)
	rec[0] = byte(rl)             // length of directory record
	rec[1] = 0                    // extended attribute record length
	putBoth32(rec[2:], extentLBA) // extent location
	putBoth32(rec[10:], size)     // data length
	copy(rec[18:25], dirRecordTime(b.created))
	if isDir {
		rec[25] = flagDirectory
	}
	// 26 file unit size, 27 interleave gap, both 0 (no interleaving).
	putBoth16(rec[28:], 1) // volume sequence number
	rec[32] = byte(len(id))
	copy(rec[33:], id)
	return rec
}

// pathTableSize returns the byte length of one path table (Type-L and Type-M
// are identical in size).
func (b *Builder) pathTableSize(dirs []*node) int {
	total := 0
	for _, d := range dirs {
		total += pathTableEntryLen(len(pathTableIdent(d)))
	}
	return total
}

// pathTable serialises a path table (ECMA-119 6.9). msb selects the Type-M
// (big-endian) ordering; otherwise Type-L (little-endian). Each entry names a
// directory, its extent LBA and its parent's path-table index.
func (b *Builder) pathTable(dirs []*node, msb bool) []byte {
	var out []byte
	for _, d := range dirs {
		id := pathTableIdent(d)
		parentIndex := d.dirIndex
		if d.parent != nil {
			parentIndex = d.parent.dirIndex
		}
		ent := make([]byte, pathTableEntryLen(len(id)))
		ent[0] = byte(len(id)) // length of directory identifier
		ent[1] = 0             // extended attribute record length
		if msb {
			putBE32(ent[2:], d.extentLBA)
			putBE16(ent[6:], uint16(parentIndex))
		} else {
			putLE32(ent[2:], d.extentLBA)
			putLE16(ent[6:], uint16(parentIndex))
		}
		copy(ent[8:], id)
		out = append(out, ent...)
	}
	// Pad the table to a whole number of sectors.
	if r := len(out) % sectorSize; r != 0 {
		out = append(out, make([]byte, sectorSize-r)...)
	}
	return out
}

// pathTableIdent returns the directory identifier used in a path-table entry:
// the single byte 0x00 for the root, else the directory's on-disk identifier.
func pathTableIdent(d *node) []byte {
	if d.parent == nil {
		return []byte{0x00}
	}
	return []byte(d.id)
}

// primaryVolumeDescriptor builds the PVD (ECMA-119 8.4) for the laid-out tree.
func (b *Builder) primaryVolumeDescriptor(dirs []*node) []byte {
	buf := make([]byte, sectorSize)
	buf[0] = vdTypePrimary
	copy(buf[1:6], standardID)
	buf[6] = 1 // volume descriptor version

	pad(buf[8:40], ' ')                            // system identifier
	copyField(buf[40:72], volumeIdent(b.volumeID)) // volume identifier

	totalSectors := b.totalSectors(dirs)
	putBoth32(buf[80:], totalSectors) // volume space size
	putBoth16(buf[120:], 1)           // volume set size
	putBoth16(buf[124:], 1)           // volume sequence number
	putBoth16(buf[128:], sectorSize)  // logical block size

	ptBytes := uint32(b.pathTableSize(dirs))
	putBoth32(buf[132:], ptBytes) // path table size
	putLE32(buf[140:], pathTableLLBA)
	// 144: optional Type-L path table — none.
	putBE32(buf[148:], b.pathTableMLBA)
	// 152: optional Type-M path table — none.

	// Root directory record (34 bytes here: 1-byte 0x00 identifier).
	copy(buf[156:190], b.dirRecord([]byte{0x00}, b.root.extentLBA, b.root.extentLen, true))

	pad(buf[190:318], ' ') // volume set identifier
	pad(buf[318:446], ' ') // publisher identifier
	pad(buf[446:574], ' ') // data preparer identifier
	pad(buf[574:702], ' ') // application identifier
	pad(buf[702:739], ' ') // copyright file identifier
	pad(buf[739:776], ' ') // abstract file identifier
	pad(buf[776:813], ' ') // bibliographic file identifier

	ts := volumeTime(b.created)
	copy(buf[813:830], ts)                // volume creation date and time
	copy(buf[830:847], ts)                // volume modification date and time
	copy(buf[847:864], volumeTimeUnset()) // expiration: none
	copy(buf[864:881], volumeTimeUnset()) // effective: none

	buf[881] = 1 // file structure version
	return buf
}

// totalSectors returns the volume space size in logical blocks: every sector
// from 0 through the end of the last file/directory extent.
func (b *Builder) totalSectors(dirs []*node) uint32 {
	max := b.pathTableMLBA + sectorsFor(uint32(b.pathTableSize(dirs)))
	end := func(lba, length uint32) {
		e := lba + sectorsFor(length)
		if e > max {
			max = e
		}
	}
	for _, d := range dirs {
		end(d.extentLBA, d.extentLen)
	}
	var walk func(d *node)
	walk = func(d *node) {
		for _, c := range sortedChildren(d) {
			if c.isDir {
				walk(c)
				continue
			}
			end(c.extentLBA, c.extentLen)
		}
	}
	walk(b.root)
	return max
}

// volumeDescriptorTerminator builds the Volume Descriptor Set Terminator
// (ECMA-119 8.3).
func volumeDescriptorTerminator() []byte {
	buf := make([]byte, sectorSize)
	buf[0] = vdTypeTerminator
	copy(buf[1:6], standardID)
	buf[6] = 1
	return buf
}

// --- small encoding helpers (write side) ---

// dirRecordLen returns the padded length of a directory record carrying an
// identifier of idLen bytes; records are an even number of bytes (a pad byte
// follows an even-length identifier, ECMA-119 9.1.12).
func dirRecordLen(idLen int) int {
	rl := dirRecordHeaderLen + idLen
	if idLen%2 == 0 {
		rl++ // padding field to keep the record length even
	}
	return rl
}

// pathTableEntryLen returns the length of a path-table entry for an identifier
// of idLen bytes (padded to even, ECMA-119 6.9.1).
func pathTableEntryLen(idLen int) int {
	n := 8 + idLen
	if idLen%2 != 0 {
		n++
	}
	return n
}

// sectorsFor returns the number of logical sectors needed for n bytes.
func sectorsFor(n uint32) uint32 { return (n + sectorSize - 1) / sectorSize }

// putBoth16 writes a 16-bit value in the both-byte-order form (little-endian
// then big-endian) used by ECMA-119 numeric fields.
func putBoth16(b []byte, v uint16) {
	putLE16(b, v)
	putBE16(b[2:], v)
}

// putBoth32 writes a 32-bit value in both-byte-order form.
func putBoth32(b []byte, v uint32) {
	putLE32(b, v)
	putBE32(b[4:], v)
}

func putLE16(b []byte, v uint16) { b[0] = byte(v); b[1] = byte(v >> 8) }
func putBE16(b []byte, v uint16) { b[0] = byte(v >> 8); b[1] = byte(v) }
func putLE32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}
func putBE32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

// pad fills b with c.
func pad(b []byte, c byte) {
	for i := range b {
		b[i] = c
	}
}

// copyField pads a field with spaces, then copies s over the start.
func copyField(b []byte, s string) {
	pad(b, ' ')
	copy(b, s)
}

// dirRecordTime encodes the 7-byte directory-record timestamp (ECMA-119
// 9.1.5): year-1900, month, day, hour, minute, second, GMT offset (15-min
// units).
func dirRecordTime(t time.Time) []byte {
	t = t.UTC()
	return []byte{
		byte(t.Year() - 1900),
		byte(t.Month()),
		byte(t.Day()),
		byte(t.Hour()),
		byte(t.Minute()),
		byte(t.Second()),
		0, // GMT offset
	}
}

// volumeTime encodes the 17-byte volume-descriptor timestamp (ECMA-119 8.4.26):
// "YYYYMMDDHHMMSShh" digits plus a 1-byte GMT offset.
func volumeTime(t time.Time) []byte {
	t = t.UTC()
	s := fmt.Sprintf("%04d%02d%02d%02d%02d%02d%02d",
		t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond()/10_000_000)
	out := make([]byte, 17)
	copy(out, s)
	out[16] = 0 // GMT offset
	return out
}

// volumeTimeUnset is the "no date specified" volume timestamp: all-zero ASCII
// digits and a zero offset (ECMA-119 8.4.26.1).
func volumeTimeUnset() []byte {
	out := make([]byte, 17)
	for i := 0; i < 16; i++ {
		out[i] = '0'
	}
	return out
}

// --- identifier construction (interchange level 1) ---

// isoIdentifier maps a caller name to a base ISO 9660 level-1 identifier:
// uppercase, restricted to A-Z 0-9 _, with a ";1" version on files and an 8.3
// shape (8-char base, 3-char extension; directories get a single 8-char name).
func isoIdentifier(name string, isDir bool) string {
	if isDir {
		return clampField(sanitize(name), 8)
	}
	base, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		base, ext = name[:i], name[i+1:]
	}
	base = clampField(sanitize(base), 8)
	if base == "" {
		base = "_"
	}
	ext = clampField(sanitize(ext), 3)
	if ext != "" {
		return base + "." + ext + ";1"
	}
	return base + ";1"
}

// disambiguate rewrites an identifier to its k-th variant by injecting a tilde
// counter into the (clamped) base, preserving any extension/version suffix.
func disambiguate(base string, isDir bool, k int) string {
	suffix := fmt.Sprintf("~%d", k)
	if isDir {
		return clampField(base, 8-len(suffix)) + suffix
	}
	name, rest := base, ""
	if i := strings.IndexByte(base, '.'); i >= 0 {
		name, rest = base[:i], base[i:]
	} else if i := strings.IndexByte(base, ';'); i >= 0 {
		name, rest = base[:i], base[i:]
	}
	return clampField(name, 8-len(suffix)) + suffix + rest
}

// sanitize uppercases name and replaces characters outside the level-1 d-char
// set (A-Z 0-9 _) with "_".
func sanitize(name string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(name) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

// clampField truncates s to at most n bytes.
func clampField(s string, n int) string {
	if n < 0 {
		n = 0
	}
	if len(s) > n {
		return s[:n]
	}
	return s
}

// volumeIdent maps a volume name to the PVD volume identifier: sanitized and
// clamped to 32 bytes, defaulting to "CDROM" when empty.
func volumeIdent(name string) string {
	id := clampField(sanitize(name), 32)
	if id == "" {
		return "CDROM"
	}
	return id
}

// --- sector writers ---

// writeSector writes a single logical sector at sector index s, zero-padding
// short data to the full sector size.
func writeSector(w io.WriterAt, s int64, data []byte) error {
	buf := make([]byte, sectorSize)
	copy(buf, data)
	return writeAt(w, s*sectorSize, buf)
}

// writeAt writes data at off, padding the tail to a whole sector so the image
// is always a multiple of the logical sector size.
func writeAt(w io.WriterAt, off int64, data []byte) error {
	if r := len(data) % sectorSize; r != 0 {
		data = append(append([]byte(nil), data...), make([]byte, sectorSize-r)...)
	}
	if _, err := w.WriteAt(data, off); err != nil {
		return fmt.Errorf("iso9660: write @offset %d: %w", off, err)
	}
	return nil
}
