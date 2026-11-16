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

func TestDirCacheIsBoundedInBytes(t *testing.T) {
	var c dirCache
	// A directory whose records carry large system-use areas: few records, many bytes.
	heavy := func() []dirRecord {
		recs := make([]dirRecord, 10)
		for i := range recs {
			recs[i].sysUse = make([]byte, maxCachedBytes/12)
		}
		return recs
	}
	c.put(dirKey{1, 1}, heavy())
	c.put(dirKey{2, 2}, heavy()) // together they exceed the budget: the first goes
	if _, ok := c.get(dirKey{1, 1}); ok {
		t.Error("the cache grew past its byte budget")
	}
	if c.bytes > maxCachedBytes {
		t.Errorf("%d bytes held, budget %d", c.bytes, maxCachedBytes)
	}
	tooBig := []dirRecord{{sysUse: make([]byte, maxCachedBytes+1)}}
	c.put(dirKey{3, 3}, tooBig)
	if _, ok := c.get(dirKey{3, 3}); ok {
		t.Error("a directory costing more than the whole budget was cached")
	}
	// One merged record (a run of multi-extent records) is charged for its extents.
	one := []dirRecord{{extents: make([]extent, maxCachedBytes/8+1)}}
	c.put(dirKey{4, 4}, one)
	if _, ok := c.get(dirKey{4, 4}); ok {
		t.Error("a record with a huge extent list was cached as if it were one small record")
	}
}

func TestListDirOfAFileIsNotServedFromTheDirectoryCache(t *testing.T) {
	img := buildDirFiles(t, 3)
	fs, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ListDir("/D"); err != nil { // caches /D
		t.Fatal(err)
	}
	rec, err := fs.resolve("/D/F0000.TXT")
	if err != nil {
		t.Fatal(err)
	}
	d, err := fs.resolve("/D")
	if err != nil {
		t.Fatal(err)
	}
	// A file record that names the cached directory's extent.
	rec.ExtentLBA, rec.Size = d.ExtentLBA, d.Size
	if _, err := fs.dirRecords(rec); err == nil {
		t.Error("a regular file was listed as the cached directory")
	}
}
