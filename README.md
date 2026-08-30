<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems-iso9660.png" alt="go-filesystems/iso9660" width="720"></p>

# iso9660

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/iso9660.svg)](https://pkg.go.dev/github.com/go-filesystems/iso9660)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-filesystems/iso9660/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/iso9660/actions/workflows/ci.yml)

Pure-Go access to **ISO 9660** (ECMA-119) CD/DVD images — no root, no external
tools, no CGO. Reading is read-only (the format has no in-place update model,
matching real optical media); authoring a fresh image is supported via
`Builder`.

ISO 9660 is the optical-disc filesystem used by `.iso` images produced by
`mkisofs` / `genisoimage` / `xorriso`. This driver reads the Primary Volume
Descriptor, walks directory records and reads file extents, exposing the image
through the shared `github.com/go-filesystems/interface` `Filesystem` API.

## Support summary

| Feature | Status | Notes |
|---|---:|---|
| Open / Close | ✅ | Primary Volume Descriptor (`CD001`) |
| ReadFile | ✅ | Single- and multi-extent files (incl. multi-sector) |
| Read at an offset | ✅ | `(*FS).OpenFile(path)` → `io.ReaderAt` + `Size()` (`filesystem.Opener` / `filesystem.File`); extent-aware, reads no data at open |
| ListDir | ✅ | Directory records; `.`/`..` filtered out |
| Stat | ✅ | Rock Ridge POSIX mode when present, else synthesised; size + extent LBA |
| Names | ✅ | Rock Ridge real names when present; else base ECMA-119 (`;version` stripped, case-insensitive) |
| Rock Ridge (POSIX names/perms/symlinks) | ✅ | `SP`/`NM`/`PX`/`SL`/`CE` continuation; deep-dir relocation (`CL`/`PL`/`RE`) not yet |
| ReadLink / symlinks | ✅ | Rock Ridge `SL` targets |
| Joliet (UCS-2 long names) | ✅ | Supplementary VD; used when Rock Ridge is absent |
| Multi-extent files | ✅ | ECMA-119 §6.5.1; consecutive records merged, extents concatenated in order |
| Author a fresh image | ✅ | `Builder` (`NewBuilder`/`AddDir`/`AddFile`/`WriteTo`) masters a base ECMA-119 image, interchange level 1; images round-trip through this package's own reader |
| Mutate an already-mastered image | ❌ | Once opened, an image is read-only — every `filesystem.Filesystem` mutator (`WriteFile`, `MkDir`, …) returns `ErrReadOnly`, matching the real on-disk format (ISO 9660 media has no in-place update model) |

## References

- ECMA-119 (ISO 9660)
- `mkisofs` / `genisoimage` / `xorriso`

## Module

```
github.com/go-filesystems/iso9660
```

## Usage

### Reading

```go
fs, err := iso9660.OpenFile("image.iso")
if err != nil { /* ... */ }
defer fs.Close()

data, err := fs.ReadFile("/BOOT/GRUB/GRUB.CFG")
entries, err := fs.ListDir("/")
```

### Reading part of a file

`ReadFile` returns the whole file, which is no use to anything that serves reads
on demand — a mount, an NFS or 9P export — where a 4 KiB request out of a
multi-gigabyte ISO must not read the lot. The driver implements the optional
[`filesystem.Opener`](https://github.com/go-filesystems/interface) capability:

```go
var generic filesystem.Filesystem = fs
if o, ok := generic.(filesystem.Opener); ok {
    f, err := o.OpenFile("/BOOT/BIG.IMG")
    if err != nil { /* ... */ }
    defer f.Close()

    buf := make([]byte, 4096)
    n, err := f.ReadAt(buf, 1<<30) // only the bytes asked for
    _, _ = n, err
    _ = f.Size()                   // from the directory record; reads nothing
}
```

`OpenFile` records where the file's extents live and reads none of them. File data
is contiguous within an extent, and a multi-extent file (ECMA-119 §6.5.1) is the
concatenation of several whose lengths are **byte counts, not block counts** — so
extent boundaries do not fall on logical-block boundaries and the offset→disk
mapping is a search, never a division. `ReadAt` follows `io.ReaderAt` exactly
(`n < len(p)` only with a non-nil error, `io.EOF` at the end) and is safe to call
concurrently.

Note the method `(*FS).OpenFile(path) (filesystem.File, error)` is distinct from
the package-level `iso9660.OpenFile(path) (*FS, error)`, which opens an image from
the host filesystem.

### Authoring a fresh image

```go
b := iso9660.NewBuilder("MYVOLUME")
_ = b.AddDir("/boot")
_ = b.AddFile("/boot/grub.cfg", []byte("..."))

f, err := os.Create("out.iso")
if err != nil { /* ... */ }
defer f.Close()
if err := b.WriteTo(f); err != nil { /* ... */ }
```

## Limitations

- An already-mastered image is read-only once opened (the on-disk format has
  no in-place update model); `Builder` masters a fresh image instead.
- `Builder` writes the base ECMA-119 layer (interchange level 1) only — Rock
  Ridge and Joliet extensions are decoded on read but not yet written.
- Rock Ridge (`SP`/`NM`/`PX`/`SL`) is decoded, including `CE` continuation
  areas; deep-directory relocation (`CL`/`PL`/`RE`) is not.
- Joliet (UCS-2 long names) is decoded from the supplementary volume descriptor
  and used when Rock Ridge is absent; without either, names appear uppercased.
- Multi-extent files (ECMA-119 §6.5.1) are read: a file recorded as several
  consecutive directory records — every record but the last carrying the
  multi-extent flag — is presented as one entry whose contents are the in-order
  concatenation of its extents.
- Intended for tooling and testing.
