# iso9660

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
| Rock Ridge (POSIX names/perms/symlinks) | ✅ | `SP`/`NM`/`PX`/`SL`; `CE` continuation + deep-dir relocation not yet |
| ReadLink / symlinks | ✅ | Rock Ridge `SL` targets |
| Joliet (UCS-2 long names) | ⏳ | Planned |
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
- Rock Ridge (`SP`/`NM`/`PX`/`SL`) is decoded; the `CE` continuation area and
  deep-directory relocation (`CL`/`PL`/`RE`) are not, so entries that overflow
  into a continuation area may be truncated.
- Joliet (UCS-2 long names via the supplementary volume descriptor) is not yet
  decoded; without Rock Ridge or Joliet, names appear uppercased.
- Multi-extent files are not supported.
- Path resolution does not follow symlinks (use `ReadLink`).
- Intended for tooling and testing.
