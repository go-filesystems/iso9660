// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func findTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, d := range []string{"/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin"} {
		c := filepath.Join(d, name)
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*29 + 5)
	}
	return b
}

// TestInterop_GenisoimageRead builds a tree, masters it into a base ISO 9660
// image with the real genisoimage, and verifies the driver reads files (incl.
// a multi-sector file), directory listings and a nested directory back. Names
// are uppercase + ";version" on disk; the driver strips the version and matches
// case-insensitively. Skipped when genisoimage/mkisofs is unavailable.
func TestInterop_GenisoimageRead(t *testing.T) {
	tool := findTool("genisoimage")
	if tool == "" {
		tool = findTool("mkisofs")
	}
	if tool == "" {
		t.Skip("genisoimage/mkisofs not available — skipping iso9660 interop test")
	}

	src := t.TempDir()
	readme := []byte("read-only optical filesystem\n")
	data := pattern(5000) // > 2 sectors: exercises multi-sector contiguous read
	nested := []byte("nested entry\n")
	files := map[string][]byte{
		"README.TXT":     readme,
		"DATA.BIN":       data,
		"SUB/NESTED.TXT": nested,
	}
	for name, content := range files {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	img := filepath.Join(t.TempDir(), "out.iso")
	cmd := exec.Command(tool, "-quiet", "-iso-level", "1", "-o", img, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", filepath.Base(tool), err, out)
	}

	fs, err := OpenFile(img)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer fs.Close()

	for name, want := range files {
		got, err := fs.ReadFile("/" + name)
		if err != nil {
			t.Errorf("ReadFile(/%s): %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("ReadFile(/%s): %d bytes, content mismatch (want %d)", name, len(got), len(want))
		}
	}

	// Case-insensitive lookup (on-disk names are uppercase).
	if got, err := fs.ReadFile("/readme.txt"); err != nil || !bytes.Equal(got, readme) {
		t.Errorf("case-insensitive ReadFile(/readme.txt): %v (equal=%v)", err, bytes.Equal(got, readme))
	}

	// Stat.
	if st, err := fs.Stat("/DATA.BIN"); err != nil {
		t.Errorf("Stat(/DATA.BIN): %v", err)
	} else if st.Size() != uint64(len(data)) {
		t.Errorf("Stat(/DATA.BIN) size = %d, want %d", st.Size(), len(data))
	}

	// Root listing.
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name()] = true
	}
	for _, name := range []string{"README.TXT", "DATA.BIN", "SUB"} {
		if !seen[name] {
			t.Errorf("ListDir(/) missing %q (got %v)", name, keys(seen))
		}
	}

	// Nested directory.
	if sub, err := fs.ListDir("/SUB"); err != nil || len(sub) != 1 || sub[0].Name() != "NESTED.TXT" {
		t.Errorf("ListDir(/SUB): err=%v len=%d; want exactly [NESTED.TXT]", err, len(sub))
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
