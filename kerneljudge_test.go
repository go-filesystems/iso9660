package iso9660

// The Linux kernel, isoinfo and xorriso as the judge of this driver, in a Tart
// VM. Skipped unless FS_KERNEL_JUDGE=1; github.com/go-filesystems/kerneljudge's
// cmd/kerneljudge sets it and runs this as root in the VM:
//
//	kerneljudge run --driver <clone of this repository> [--perf]

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
	kj "github.com/go-filesystems/kerneljudge"
)

var isoPlain = regexp.MustCompile(`^[a-z0-9_]{1,8}$`)

// isoComponent maps a name to ISO 9660 level 1 (the only level the writer
// emits): upper-case d-characters, 8 characters and a 3-character extension.
// A name that does not fit becomes X plus 7 hex digits, so UTF-8 names are
// outside what this format (without Joliet) can hold.
func isoComponent(name string, file bool) string {
	base, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); file && i > 0 {
		base, ext = name[:i], name[i+1:]
	}
	if !isoPlain.MatchString(base) {
		h := fnv.New32a()
		h.Write([]byte(base))
		base = strings.ToLower(fmt.Sprintf("x%07x", h.Sum32()&0xfffffff))
	}
	out := strings.ToUpper(base)
	if file && ext != "" {
		if len(ext) > 3 || !isoPlain.MatchString(ext) {
			ext = "dat"
		}
		out += "." + strings.ToUpper(ext)
	}
	return out
}

func isoMap(e kj.Entry) (kj.Entry, bool) {
	parts := strings.Split(strings.TrimPrefix(e.Path, "/"), "/")
	for i := range parts {
		parts[i] = isoComponent(parts[i], i == len(parts)-1 && e.Kind == kj.File)
	}
	e.Path = "/" + strings.Join(parts, "/")
	return e, true
}

// buildISO masters srcDir with our Builder.
func buildISO(img, src string) error {
	b := NewBuilder("KERNELJUDGE")
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == src {
			return err
		}
		rel := "/" + filepath.ToSlash(strings.TrimPrefix(p, src+string(filepath.Separator)))
		if d.IsDir() {
			return b.AddDir(rel)
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return b.AddFile(rel, data)
	})
	if err != nil {
		return err
	}
	f, err := os.Create(img)
	if err != nil {
		return err
	}
	if err := b.WriteTo(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func kernelJudgeDriver() kj.Driver {
	// Base ISO 9660: no symlinks, xattrs, permissions or times we can compare.
	caps := kj.Caps{}
	return kj.Driver{
		Name:   "iso9660",
		FSType: "iso9660",
		Open: func(img string) (filesystem.Filesystem, error) {
			f, err := OpenFile(img)
			if err != nil {
				return nil, err
			}
			return f, nil
		},
		Builders: []kj.Builder{{Name: "builder", Build: buildISO}},
		Natives: []kj.Native{
			{Name: "xorriso-level1", Args: []string{"xorriso", "-as", "mkisofs", "-quiet", "-iso-level", "1", "-o", "{img}", "{src}"}},
			{Name: "genisoimage-level1", Args: []string{"genisoimage", "-quiet", "-iso-level", "1", "-o", "{img}", "{src}"}},
		},
		Fsck: [][]string{
			{"isoinfo", "-d", "-i", "{img}"},
			{"isoinfo", "-l", "-i", "{img}"},
		},
		FSCaps:  &caps,
		Map:     isoMap,
		Compare: kj.Compare{FoldCase: true},
	}
}

func TestKernelJudge(t *testing.T)     { kj.Run(t, kernelJudgeDriver()) }
func TestKernelJudgePerf(t *testing.T) { kj.RunPerf(t, kernelJudgeDriver(), kj.PerfOptions{}) }
