package iso9660

import (
	"bytes"
	"fmt"
	"io"
	"testing"
)

type dirReadCounter struct {
	r io.ReaderAt
	n int
}

func (c *dirReadCounter) ReadAt(p []byte, off int64) (int, error) {
	c.n++
	return c.r.ReadAt(p, off)
}

type dirMemWriter struct{ b []byte }

func (m *dirMemWriter) WriteAt(p []byte, off int64) (int, error) {
	if end := int(off) + len(p); end > len(m.b) {
		m.b = append(m.b, make([]byte, end-len(m.b))...)
	}
	return copy(m.b[off:], p), nil
}

func buildDirFiles(t *testing.T, n int) []byte {
	t.Helper()
	b := NewBuilder("TEST")
	for i := 0; i < n; i++ {
		if err := b.AddFile(fmt.Sprintf("/D/F%04d.TXT", i), []byte(fmt.Sprintf("file %d", i))); err != nil {
			t.Fatal(err)
		}
	}
	w := &dirMemWriter{}
	if err := b.WriteTo(w); err != nil {
		t.Fatal(err)
	}
	return w.b
}

// Every Stat resolves its path by reading and parsing the directory of each
// component; a repeated Stat must cost no device read once the directories are
// cached (#33).
func TestStatOfKnownDirectoriesCostsNoDeviceRead(t *testing.T) {
	img := buildDirFiles(t, 80)
	cr := &dirReadCounter{r: bytes.NewReader(img)}
	fs, err := Open(cr, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 80; i++ { // warm
		if _, err := fs.Stat(fmt.Sprintf("/D/F%04d.TXT", i)); err != nil {
			t.Fatal(err)
		}
	}
	before := cr.n
	for i := 0; i < 80; i++ {
		st, err := fs.Stat(fmt.Sprintf("/D/F%04d.TXT", i))
		if err != nil || st.Size() != uint64(len(fmt.Sprintf("file %d", i))) {
			t.Fatalf("file %d: %v", i, err)
		}
	}
	if reads := cr.n - before; reads != 0 {
		t.Errorf("80 Stats of known paths cost %d device reads: the parsed directories are not cached", reads)
	}
}

func TestDirCacheIsBounded(t *testing.T) {
	var c dirCache
	big := make([]dirRecord, maxCachedRecords/2+1)
	c.put(dirKey{1, 1}, big)
	c.put(dirKey{2, 2}, big) // evicts the first: both would exceed the budget
	if _, ok := c.get(dirKey{1, 1}); ok {
		t.Error("the cache grew past its record budget")
	}
	if c.records > maxCachedRecords {
		t.Errorf("%d records held, budget %d", c.records, maxCachedRecords)
	}
	c.put(dirKey{3, 3}, make([]dirRecord, maxCachedRecords+1))
	if _, ok := c.get(dirKey{3, 3}); ok {
		t.Error("a directory larger than the whole budget was cached")
	}
}
