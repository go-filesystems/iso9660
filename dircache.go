// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package iso9660

import (
	"slices"
	"sync"
)

// dirCache keeps the parsed records of recently read directories. The image is
// read-only, so a directory extent never changes while it is open. Every Stat,
// ReadFile and ListDir resolves its path by reading and parsing the directory
// of each component; without the cache a walk of N files parsed the same
// directories N times over (a metadata walk was 1.27x slower than the kernel,
// #33).
//
// It is bounded in BYTES, estimated from what the records hold (a record is a
// struct plus its name, system-use and extent bytes): a directory extent is up
// to maxDirExtent bytes of an attacker-controlled image, and a run of
// multi-extent records merges into one large record, so neither a count of
// directories nor a count of records bounds memory. A directory costing more
// than the whole budget is not cached. Eviction is FIFO.
type dirCache struct {
	mu    sync.Mutex
	dirs  map[dirKey][]dirRecord
	order []dirKey
	cost  map[dirKey]int
	bytes int
}

type dirKey struct {
	lba  uint32
	size uint32
}

// maxCachedBytes bounds the cache.
const maxCachedBytes = 32 << 20

// recordSlot is the size charged for one dirRecord struct (its fields, slice
// headers and string header, rounded up).
const recordSlot = 128

// dirCost estimates the memory a parsed directory holds.
func dirCost(recs []dirRecord) int {
	n := len(recs) * recordSlot
	for _, r := range recs {
		n += len(r.rawName) + len(r.Name) + len(r.sysUse) + 8*len(r.extents)
	}
	return n
}

func (c *dirCache) get(k dirKey) ([]dirRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.dirs[k]
	return r, ok
}

func (c *dirCache) put(k dirKey, recs []dirRecord) {
	recs = slices.Clip(recs) // mergeMultiExtent sized the slice for the unmerged records
	cost := dirCost(recs)
	if cost > maxCachedBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dirs == nil {
		c.dirs = make(map[dirKey][]dirRecord)
		c.cost = make(map[dirKey]int)
	}
	if _, ok := c.dirs[k]; ok {
		return
	}
	for c.bytes+cost > maxCachedBytes && len(c.order) > 0 {
		old := c.order[0]
		c.order = c.order[1:]
		c.bytes -= c.cost[old]
		delete(c.dirs, old)
		delete(c.cost, old)
	}
	c.dirs[k] = recs
	c.cost[k] = cost
	c.order = append(c.order, k)
	c.bytes += cost
}

// dirRecords is readDirRecords through the cache. The returned slice is shared
// and must not be modified. A record that is not a directory is refused before
// the lookup: a file whose (LBA, size) equals a cached directory's must not be
// listed as that directory.
func (fs *FS) dirRecords(rec dirRecord) ([]dirRecord, error) {
	if !rec.isDir() {
		return nil, ErrNotDirectory
	}
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
