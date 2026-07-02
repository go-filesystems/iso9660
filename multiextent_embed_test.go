// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import (
	"bytes"
	_ "embed"
	"testing"
)

// multiExtentISO is a hand-built ECMA-119 image embedded at compile time.
//
// A real mastering tool (mkisofs/xorriso) only splits a file across multiple
// directory records — the multi-extent form of ECMA-119 §6.5.1 — when the file
// exceeds a single extent's 4 GiB byte-length field, which would make any
// genuine fixture larger than 4 GiB and impossible to embed. The image is
// therefore hand-built (its generator lives in the commit history under
// testdata/) yet is a real on-disk ISO byte stream: the whole read path, from
// Open through the volume descriptors and directory records to file extents,
// runs against it exactly as it would against a mastered disc.
//
// It is embedded rather than read from a relative path so the test is
// self-contained on every target — emulated CI arches run the test binary from
// a working directory that need not contain testdata/.
//
// Layout: BIG.BIN is three consecutive directory records (two carrying the
// multi-extent flag, one final) whose extents concatenate to A||B||C; PLAIN.TXT
// is an ordinary single-extent sibling that immediately follows the run, so the
// fixture also proves the merge stops at the final extent and does not swallow
// the next entry.
//
//go:embed testdata/multiextent.iso
var multiExtentISO []byte

func TestMultiExtentEmbeddedFixture(t *testing.T) {
	fs, err := Open(bytes.NewReader(multiExtentISO), int64(len(multiExtentISO)))
	if err != nil {
		t.Fatalf("Open embedded fixture: %v", err)
	}

	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	if len(entries) != 2 || !got["BIG.BIN"] || !got["PLAIN.TXT"] {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("ListDir(/) = %v; want exactly BIG.BIN and PLAIN.TXT", names)
	}

	// BIG.BIN is A(1500×0xAA) || B(2048×0xBB) || C(700×0xCC).
	want := bytes.Join([][]byte{
		bytes.Repeat([]byte{0xAA}, 1500),
		bytes.Repeat([]byte{0xBB}, 2048),
		bytes.Repeat([]byte{0xCC}, 700),
	}, nil)
	big, err := fs.ReadFile("/BIG.BIN")
	if err != nil {
		t.Fatalf("ReadFile(/BIG.BIN): %v", err)
	}
	if !bytes.Equal(big, want) {
		t.Fatalf("ReadFile(/BIG.BIN): %d bytes, content mismatch (want %d)", len(big), len(want))
	}
	if st, err := fs.Stat("/BIG.BIN"); err != nil {
		t.Errorf("Stat(/BIG.BIN): %v", err)
	} else if st.Size() != uint64(len(want)) {
		t.Errorf("Stat(/BIG.BIN) size = %d, want %d", st.Size(), len(want))
	}

	// The single-extent sibling must read back unaffected by the merge.
	plain, err := fs.ReadFile("/PLAIN.TXT")
	if err != nil {
		t.Fatalf("ReadFile(/PLAIN.TXT): %v", err)
	}
	if wantP := []byte("ordinary single-extent sibling\n"); !bytes.Equal(plain, wantP) {
		t.Fatalf("ReadFile(/PLAIN.TXT) = %q, want %q", plain, wantP)
	}
}
