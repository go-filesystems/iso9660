// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import "sync"

// dirCache keeps the parsed records of recently read directories. The image is
// read-only, so a directory extent never changes while it is open. Every Stat,
// ReadFile and ListDir resolves its path by reading and parsing the directory
// of each component; without the cache a walk of N files parsed the same
// directories N times over (a metadata walk was 1.27x slower than the kernel,
// #33).
//
// It is bounded by the number of records held, not by directories: a directory
// is at most maxDirExtent bytes of records and the image is attacker-controlled,
// so a count of directories would not bound memory. A directory holding more
// records than the whole budget is simply not cached. Eviction is FIFO.
type dirCache struct {
	mu      sync.Mutex
	dirs    map[dirKey][]dirRecord
	order   []dirKey
	records int
}

type dirKey struct {
	lba  uint32
	size uint32
}

// maxCachedRecords bounds the cache: about 40 bytes of header plus the name and
// system-use bytes of a record, so a few tens of MiB at the most.
const maxCachedRecords = 1 << 16

func (c *dirCache) get(k dirKey) ([]dirRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.dirs[k]
	return r, ok
}

func (c *dirCache) put(k dirKey, recs []dirRecord) {
	if len(recs) > maxCachedRecords {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dirs == nil {
		c.dirs = make(map[dirKey][]dirRecord)
	}
	if _, ok := c.dirs[k]; ok {
		return
	}
	for c.records+len(recs) > maxCachedRecords && len(c.order) > 0 {
		old := c.order[0]
		c.order = c.order[1:]
		c.records -= len(c.dirs[old])
		delete(c.dirs, old)
	}
	c.dirs[k] = recs
	c.order = append(c.order, k)
	c.records += len(recs)
}

// dirRecords is readDirRecords through the cache. The returned slice is shared
// and must not be modified.
func (fs *FS) dirRecords(rec dirRecord) ([]dirRecord, error) {
	k := dirKey{rec.ExtentLBA, rec.Size}
	if r, ok := fs.dirs.get(k); ok {
		return r, nil
	}
	r, err := readDirRecords(fs.rs, fs.vol, rec, allocCeiling(fs.size))
	if err != nil {
		return nil, err
	}
	fs.dirs.put(k, r)
	return r, nil
}
