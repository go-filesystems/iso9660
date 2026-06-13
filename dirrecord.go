// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"
)

// File-flag bits in a directory record (offset 25).
const (
	flagHidden    = 0x01
	flagDirectory = 0x02
	flagAssoc     = 0x04
	flagMultiExt  = 0x80 // record is not the final extent of the file
)

// dirRecord is a decoded ISO 9660 directory record.
type dirRecord struct {
	Length    uint8  // total on-disk record length; 0 marks end-of-sector padding
	ExtentLBA uint32 // logical block address of the file/dir extent
	Size      uint32 // data length in bytes
	Flags     uint8
	rawName   []byte // raw file identifier
	Name      string // cleaned name (version + trailing dot stripped)
	sysUse    []byte // System Use Area (SUSP / Rock Ridge entries)
}

func (r dirRecord) isDir() bool { return r.Flags&flagDirectory != 0 }

// isSpecial reports whether the record is the "." (0x00) or ".." (0x01) entry.
func (r dirRecord) isSpecial() bool {
	return len(r.rawName) == 1 && (r.rawName[0] == 0x00 || r.rawName[0] == 0x01)
}

// parseDirRecord decodes the directory record at the start of buf and returns
// it plus the number of bytes consumed. A leading zero length means the record
// slot is empty (sector padding); the caller should advance to the next sector.
func parseDirRecord(buf []byte) (dirRecord, int, error) {
	if len(buf) < 1 || buf[0] == 0 {
		return dirRecord{Length: 0}, 0, nil
	}
	recLen := int(buf[0])
	if recLen < 33 || recLen > len(buf) {
		return dirRecord{}, 0, fmt.Errorf("%w: dir record length %d", ErrCorrupt, recLen)
	}
	nameLen := int(buf[32])
	if 33+nameLen > recLen {
		return dirRecord{}, 0, fmt.Errorf("%w: name length %d overflows record %d", ErrCorrupt, nameLen, recLen)
	}
	rec := dirRecord{
		Length:    buf[0],
		ExtentLBA: le32(buf[2:]),
		Size:      le32(buf[10:]),
		Flags:     buf[25],
		rawName:   append([]byte(nil), buf[33:33+nameLen]...),
	}
	rec.Name = cleanName(rec.rawName)
	// The System Use Area follows the name, plus a padding byte when the name
	// length is even (16-bit field alignment).
	suaStart := 33 + nameLen
	if nameLen%2 == 0 {
		suaStart++
	}
	if suaStart < recLen {
		rec.sysUse = append([]byte(nil), buf[suaStart:recLen]...)
	}
	return rec, recLen, nil
}

// cleanName converts an ISO 9660 file identifier into a usable name: it strips
// the ";version" suffix and a trailing "." left after a versionless extension.
// The "." and ".." special entries are returned as those literals.
func cleanName(raw []byte) string {
	if len(raw) == 1 {
		switch raw[0] {
		case 0x00:
			return "."
		case 0x01:
			return ".."
		}
	}
	s := string(raw)
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, ".")
	return s
}

// jolietName decodes a Joliet directory-record identifier, which is stored as
// big-endian UCS-2. The "." and ".." special entries remain single bytes.
func jolietName(raw []byte) string {
	if len(raw) == 1 {
		switch raw[0] {
		case 0x00:
			return "."
		case 0x01:
			return ".."
		}
	}
	u := make([]uint16, len(raw)/2)
	for i := range u {
		u[i] = binary.BigEndian.Uint16(raw[2*i:])
	}
	s := string(utf16.Decode(u))
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSuffix(s, ".")
}

func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
