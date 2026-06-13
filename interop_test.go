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

// TestInterop_RockRidgeRead masters a tree with genisoimage -R (Rock Ridge) so
// the image carries real (case-preserved, long) names, POSIX modes and a
// symlink, then verifies the driver surfaces them: ReadFile by real name,
// ListDir real names, Stat mode bits, and ReadLink target.
func TestInterop_RockRidgeRead(t *testing.T) {
	tool := findTool("genisoimage")
	if tool == "" {
		tool = findTool("mkisofs")
	}
	if tool == "" {
		t.Skip("genisoimage/mkisofs not available")
	}

	src := t.TempDir()
	long := []byte("rock ridge keeps long, MixedCase names\n")
	files := map[string][]byte{
		"MixedCase Long Name.txt": long,
		"dir/inner-file.md":       []byte("# nested\n"),
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
	if err := os.Symlink("MixedCase Long Name.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	img := filepath.Join(t.TempDir(), "rr.iso")
	cmd := exec.Command(tool, "-quiet", "-R", "-o", img, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s -R: %v\n%s", filepath.Base(tool), err, out)
	}

	fs, err := OpenFile(img)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer fs.Close()

	// Real (case-preserved, spaced) names round-trip.
	for name, want := range files {
		got, err := fs.ReadFile("/" + name)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("ReadFile(/%s): err=%v equal=%v", name, err, bytes.Equal(got, want))
		}
	}

	// Listing surfaces the real names.
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name()] = true
	}
	for _, name := range []string{"MixedCase Long Name.txt", "dir", "link"} {
		if !seen[name] {
			t.Errorf("ListDir(/) missing %q (got %v)", name, keys(seen))
		}
	}

	// Symlink target via Rock Ridge SL.
	if tgt, err := fs.ReadLink("/link"); err != nil || tgt != "MixedCase Long Name.txt" {
		t.Errorf("ReadLink(/link) = %q, %v; want %q", tgt, err, "MixedCase Long Name.txt")
	}

	// PX mode: a regular file reports S_IFREG.
	if st, err := fs.Stat("/MixedCase Long Name.txt"); err != nil {
		t.Errorf("Stat: %v", err)
	} else if st.Mode()&0xF000 != sIFREG {
		t.Errorf("Stat mode = 0x%04x, want S_IFREG bits", st.Mode())
	}
}

// TestInterop_RockRidgeCE forces a Rock Ridge SUSP continuation area: a
// symlink whose target is long enough that its SL entry (plus NM/PX) overflows
// the directory record's System Use Area into a CE block. Without CE handling
// the target reads back truncated.
func TestInterop_RockRidgeCE(t *testing.T) {
	tool := findTool("genisoimage")
	if tool == "" {
		tool = findTool("mkisofs")
	}
	if tool == "" {
		t.Skip("genisoimage/mkisofs not available")
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "target.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// ~230-char relative target: long enough to spill SUSP into a CE area.
	longTarget := ""
	for i := 0; i < 23; i++ {
		longTarget += "abcdef/../" // 10 chars each → 230
	}
	longTarget += "target.txt"
	if err := os.Symlink(longTarget, filepath.Join(src, "deeplink")); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(t.TempDir(), "ce.iso")
	if out, err := exec.Command(tool, "-quiet", "-R", "-o", img, src).CombinedOutput(); err != nil {
		t.Fatalf("%s -R: %v\n%s", filepath.Base(tool), err, out)
	}
	fs, err := OpenFile(img)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer fs.Close()
	got, err := fs.ReadLink("/deeplink")
	if err != nil || got != longTarget {
		t.Errorf("ReadLink(/deeplink) = %q (len %d), %v; want len %d", got, len(got), err, len(longTarget))
	}
}

// TestInterop_RockRidgeSymlinkFollow verifies that path resolution follows a
// Rock Ridge symlink to a directory mid-path (ReadFile/Stat through it), while
// ReadLink on the link still returns the target unfollowed.
func TestInterop_RockRidgeSymlinkFollow(t *testing.T) {
	tool := findTool("genisoimage")
	if tool == "" {
		tool = findTool("mkisofs")
	}
	if tool == "" {
		t.Skip("genisoimage/mkisofs not available")
	}
	src := t.TempDir()
	want := []byte("reached through a symlinked directory\n")
	if err := os.MkdirAll(filepath.Join(src, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "real", "data.txt"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	// "alias" -> "real" (relative symlink to a directory).
	if err := os.Symlink("real", filepath.Join(src, "alias")); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(t.TempDir(), "sl.iso")
	if out, err := exec.Command(tool, "-quiet", "-R", "-o", img, src).CombinedOutput(); err != nil {
		t.Fatalf("%s -R: %v\n%s", filepath.Base(tool), err, out)
	}
	fs, err := OpenFile(img)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer fs.Close()

	// ReadFile traverses through the symlinked directory.
	if got, err := fs.ReadFile("/alias/data.txt"); err != nil || !bytes.Equal(got, want) {
		t.Errorf("ReadFile(/alias/data.txt): err=%v equal=%v", err, bytes.Equal(got, want))
	}
	// ListDir through the symlinked directory.
	if entries, err := fs.ListDir("/alias"); err != nil || len(entries) != 1 || entries[0].Name() != "data.txt" {
		t.Errorf("ListDir(/alias): err=%v len=%d", err, len(entries))
	}
	// ReadLink does not follow the final component.
	if tgt, err := fs.ReadLink("/alias"); err != nil || tgt != "real" {
		t.Errorf("ReadLink(/alias) = %q, %v; want %q", tgt, err, "real")
	}
}

// TestInterop_JolietRead masters a tree with genisoimage -J (Joliet, no Rock
// Ridge), so real long/mixed-case names live only in the UCS-2 Joliet tree.
// The driver must pick the Joliet tree and decode the names.
func TestInterop_JolietRead(t *testing.T) {
	tool := findTool("genisoimage")
	if tool == "" {
		tool = findTool("mkisofs")
	}
	if tool == "" {
		t.Skip("genisoimage/mkisofs not available")
	}

	src := t.TempDir()
	want := []byte("joliet stores UCS-2 long names\n")
	files := map[string][]byte{
		"Joliet Long Name.txt": want,
		"Folder/Inner File.md": []byte("# inner\n"),
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

	img := filepath.Join(t.TempDir(), "joliet.iso")
	if out, err := exec.Command(tool, "-quiet", "-J", "-o", img, src).CombinedOutput(); err != nil {
		t.Fatalf("%s -J: %v\n%s", filepath.Base(tool), err, out)
	}

	fs, err := OpenFile(img)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer fs.Close()
	if !fs.joliet {
		t.Fatalf("expected the Joliet tree to be selected")
	}

	for name, content := range files {
		got, err := fs.ReadFile("/" + name)
		if err != nil || !bytes.Equal(got, content) {
			t.Errorf("ReadFile(/%s): err=%v equal=%v", name, err, bytes.Equal(got, content))
		}
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name()] = true
	}
	for _, name := range []string{"Joliet Long Name.txt", "Folder"} {
		if !seen[name] {
			t.Errorf("ListDir(/) missing %q (got %v)", name, keys(seen))
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
