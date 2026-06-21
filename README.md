<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems-iso9660.png" alt="go-filesystems/iso9660" width="720"></p>

# iso9660

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/iso9660.svg)](https://pkg.go.dev/github.com/go-filesystems/iso9660)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-filesystems/iso9660/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/iso9660/actions/workflows/ci.yml)

Pure-Go, read-only access to **ISO 9660** (ECMA-119) CD/DVD images — no root, no external tools, no CGO.

ISO 9660 is the optical-disc filesystem used by `.iso` images produced by
`mkisofs` / `genisoimage` / `xorriso`. This driver reads the Primary Volume
Descriptor, walks directory records and reads file extents, exposing the image
through the shared `github.com/go-filesystems/interface` `Filesystem` API.

## Support summary

| Feature | Status | Notes |
|---|---:|---|
| Open / Close | ✅ | Primary Volume Descriptor (`CD001`) |
| ReadFile | ✅ | Contiguous single-extent files (incl. multi-sector) |
| ListDir | ✅ | Directory records; `.`/`..` filtered out |
| Stat | ✅ | Rock Ridge POSIX mode when present, else synthesised; size + extent LBA |
| Names | ✅ | Rock Ridge real names when present; else base ECMA-119 (`;version` stripped, case-insensitive) |
| Rock Ridge (POSIX names/perms/symlinks) | ✅ | `SP`/`NM`/`PX`/`SL`/`CE` continuation; deep-dir relocation (`CL`/`PL`/`RE`) not yet |
| ReadLink / symlinks | ✅ | Rock Ridge `SL` targets |
| Joliet (UCS-2 long names) | ✅ | Supplementary VD; used when Rock Ridge is absent |
| Multi-extent files | ❌ | Returns an error (planned) |
| Write operations | ❌ | Read-only format; mutators return `ErrReadOnly` |

## References

- ECMA-119 (ISO 9660)
- `mkisofs` / `genisoimage` / `xorriso`

## Module

```
github.com/go-filesystems/iso9660
```

## Usage

```go
fs, err := iso9660.OpenFile("image.iso")
if err != nil { /* ... */ }
defer fs.Close()

data, err := fs.ReadFile("/BOOT/GRUB/GRUB.CFG")
entries, err := fs.ListDir("/")
```

## Limitations

- Read-only (the on-disk format is read-only by design).
- Rock Ridge (`SP`/`NM`/`PX`/`SL`) is decoded, including `CE` continuation
  areas; deep-directory relocation (`CL`/`PL`/`RE`) is not.
- Joliet (UCS-2 long names) is decoded from the supplementary volume descriptor
  and used when Rock Ridge is absent; without either, names appear uppercased.
- Multi-extent files are not supported.
- Intended for tooling and testing.
