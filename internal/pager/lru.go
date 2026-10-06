package pager

import "container/list"

// cacheKey identifies one committed version of a page: the WAL frame it came
// from, or frame 0 for the copy in the database file.
type cacheKey struct {
	page  uint32
	frame uint64
}

type lruEntry struct {
	key  cacheKey
	data []byte
}

// lru is the buffer pool: a fixed-size least-recently-used page cache.
// Entries are immutable page images, so they can be shared with readers.
type lru struct {
	cap   int
	ll    *list.List
	items map[cacheKey]*list.Element
}

func newLRU(capacity int) *lru {
	return &lru{cap: capacity, ll: list.New(), items: map[cacheKey]*list.Element{}}
}

func (c *lru) get(k cacheKey) ([]byte, bool) {
	if e, ok := c.items[k]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*lruEntry).data, true
	}
	return nil, false
}

func (c *lru) put(k cacheKey, data []byte) {
	if e, ok := c.items[k]; ok {
		e.Value.(*lruEntry).data = data
		c.ll.MoveToFront(e)
		return
	}
	c.items[k] = c.ll.PushFront(&lruEntry{key: k, data: data})
	for c.ll.Len() > c.cap {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.items, last.Value.(*lruEntry).key)
	}
}

func (c *lru) len() int { return c.ll.Len() }

func (c *lru) clear() {
	c.ll.Init()
	c.items = map[cacheKey]*list.Element{}
}
