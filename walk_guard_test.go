package iso9660

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/go-volumes/safeio"
)

// TestSymlinkFanoutIsRefused: a chain of Rock Ridge symlinks k -> "k+1/k+1",
// the last one -> ".", doubles the lookups at every level. Depth is bounded by
// the hop limit, but the TOTAL number of links followed must be bounded too:
// the lookup is refused once the shared budget is spent.
func TestSymlinkFanoutIsRefused(t *testing.T) {
	const (
		pvdLBA      = 16
		termLBA     = 17
		rootLBA     = 18
		totalBlocks = 19
		depth       = 34
	)
	names := "abcdefghijklmnopqrstuvwxyz0123456789"
	img := make([]byte, totalBlocks*sectorSize)
	pvd := img[pvdLBA*sectorSize:]
	pvd[0] = vdTypePrimary
	copy(pvd[1:6], standardID)
	binary.LittleEndian.PutUint32(pvd[80:], totalBlocks)
	binary.LittleEndian.PutUint16(pvd[128:], sectorSize)
	copy(pvd[156:], buildDirRecord(rootLBA, sectorSize, flagDirectory, []byte{0x00}))
	term := img[termLBA*sectorSize:]
	term[0] = vdTypeTerminator
	copy(term[1:6], standardID)

	root := img[rootLBA*sectorSize : (rootLBA+1)*sectorSize]
	pos := 0
	put := func(rec []byte) {
		if pos+len(rec) > len(root) {
			t.Fatalf("directory sector overflow at %d", pos)
		}
		copy(root[pos:], rec)
		pos += len(rec)
	}
	put(appendSUA(buildDirRecord(rootLBA, sectorSize, flagDirectory, []byte{0x00}), spEntry()))
	put(buildDirRecord(rootLBA, sectorSize, flagDirectory, []byte{0x01}))
	for k := 0; k < depth; k++ {
		target := "."
		if k < depth-1 {
			n := string(names[k+1])
			target = n + "/" + n
		}
		base := []byte{'A' + byte(k/26), 'A' + byte(k%26)}
		put(appendSUA(
			buildDirRecord(rootLBA, 0, 0x00, base),
			append(nmEntry(string(names[k])), slEntry(target)...),
		))
	}

	fs, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Control: the tail of the chain resolves within the budget.
	if _, err := fs.ListDir("/" + string(names[depth-1])); err != nil {
		t.Fatalf("tail link: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := fs.ListDir("/" + string(names[0]))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrTooManyLinks) || !errors.Is(err, safeio.ErrSymlinkLoop) {
			t.Fatalf("ListDir(/a) = %v, want ErrTooManyLinks wrapping safeio.ErrSymlinkLoop", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("ListDir(/a) still running after 20s: symlink fan-out is not bounded")
	}
}
