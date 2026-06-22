// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import "encoding/binary"

// Rock Ridge / SUSP support.
//
// The System Use Area (SUA) that trails a directory record's name carries a
// sequence of SUSP entries: a 2-byte signature, a 1-byte length (covering the
// whole entry), a 1-byte version, then payload. Rock Ridge (RRIP) adds the
// entries this driver decodes:
//
//   - SP — present in the root "." entry; declares LEN_SKP bytes to skip at the
//          start of every SUA.
//   - NM — alternate (real, case-preserved, long) name; may span entries via a
//          CONTINUE flag.
//   - PX — POSIX file mode (and link count / uid / gid, which we ignore).
//   - SL — symbolic-link target, assembled from components, possibly across
//          multiple entries.
//
// CE (continuation into another extent), and the CL/PL/RE deep-directory
// relocation entries, are not decoded; entries that overflow into a CE area are
// silently truncated. genisoimage -R / mkisofs -R images with short names and
// targets do not need them.

// rrInfo holds the Rock Ridge attributes extracted from a record's SUA.
type rrInfo struct {
	name      string
	hasName   bool
	mode      uint16
	hasMode   bool
	symlink   string
	isSymlink bool
}

// SL component flags.
const (
	slContinue = 0x01 // this SL entry continues in the next SL entry
	slCompCont = 0x01 // this component continues in the next component
	slCompCur  = 0x02 // "."
	slCompPar  = 0x04 // ".."
	slCompRoot = 0x08 // "/"
)

const nmContinue = 0x01 // NM name continues in the next NM entry

// ceEntry locates a SUSP CE (continuation area) entry within a System Use
// Area: a block location, an offset within that block, and a length. Returns
// found=false when no CE entry is present.
func ceEntry(sua []byte) (block uint32, offset, length uint32, found bool) {
	o := 0
	for o+4 <= len(sua) {
		sig0, sig1 := sua[o], sua[o+1]
		l := int(sua[o+2])
		if l < 4 || o+l > len(sua) {
			break
		}
		// CE payload: block(both-endian u32 @0), offset(@8), length(@16).
		if sig0 == 'C' && sig1 == 'E' && l >= 28 {
			d := sua[o+4 : o+l]
			return le32(d[0:]), le32(d[8:]), le32(d[16:]), true
		}
		o += l
	}
	return 0, 0, 0, false
}

// detectSUSPSkip inspects a root "." SUA for an SP entry and returns its
// LEN_SKP (bytes to skip at the start of every SUA) and whether SP was found.
// SP presence is SUSP's signal that the System Use Sharing Protocol — and
// therefore Rock Ridge — is in use.
func detectSUSPSkip(sua []byte) (skip int, found bool) {
	o := 0
	for o+4 <= len(sua) {
		sig0, sig1 := sua[o], sua[o+1]
		length := int(sua[o+2])
		if length < 4 || o+length > len(sua) {
			break
		}
		if sig0 == 'S' && sig1 == 'P' && length >= 7 {
			// data: check bytes 0xBE 0xEF, then LEN_SKP.
			if sua[o+4] == 0xBE && sua[o+5] == 0xEF {
				return int(sua[o+6]), true
			}
		}
		o += length
	}
	return 0, false
}

// parseRockRidge decodes the NM / PX / SL entries from a SUA (already advanced
// past any SP skip).
func parseRockRidge(sua []byte) rrInfo {
	var rr rrInfo
	var slTarget []byte
	var slComp []byte // accumulates a component split across SL entries

	o := 0
	for o+4 <= len(sua) {
		sig0, sig1 := sua[o], sua[o+1]
		length := int(sua[o+2])
		if length < 4 || o+length > len(sua) {
			break
		}
		data := sua[o+4 : o+length]
		switch {
		case sig0 == 'N' && sig1 == 'M' && len(data) >= 1:
			rr.name += string(data[1:])
			rr.hasName = true
			// If CONTINUE is not set, the name is complete (we still allow a
			// later NM to extend, which standard images won't produce).
			_ = data[0]&nmContinue != 0

		case sig0 == 'P' && sig1 == 'X' && len(data) >= 4:
			rr.mode = uint16(binary.LittleEndian.Uint32(data[0:4]))
			rr.hasMode = true

		case sig0 == 'S' && sig1 == 'L' && len(data) >= 1:
			rr.isSymlink = true
			comps := data[1:]
			ci := 0
			for ci+2 <= len(comps) {
				cflags := comps[ci]
				clen := int(comps[ci+1])
				if ci+2+clen > len(comps) {
					break
				}
				content := comps[ci+2 : ci+2+clen]
				switch {
				case cflags&slCompCur != 0:
					slComp = append(slComp, '.')
				case cflags&slCompPar != 0:
					slComp = append(slComp, '.', '.')
				case cflags&slCompRoot != 0:
					// Root component: target is absolute.
					if len(slTarget) == 0 && len(slComp) == 0 {
						slTarget = append(slTarget, '/')
						ci += 2 + clen
						continue
					}
				default:
					slComp = append(slComp, content...)
				}
				if cflags&slCompCont == 0 {
					// Component complete: flush it as a path element.
					if len(slTarget) > 0 && slTarget[len(slTarget)-1] != '/' {
						slTarget = append(slTarget, '/')
					}
					slTarget = append(slTarget, slComp...)
					slComp = slComp[:0]
				}
				ci += 2 + clen
			}
		}
		o += length
	}
	if rr.isSymlink {
		rr.symlink = string(slTarget)
	}
	return rr
}
