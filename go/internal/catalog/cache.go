package catalog

import "container/list"

// entryCache is a fixed-size LRU in front of the entries table.
//
// The hot path for a cache hit is one entry lookup, and a build farm re-requests the
// same handful of keys relentlessly. Keeping those in memory means the common case
// never reaches SQLite at all, which is what makes the pure-Go driver's slower
// per-query cost irrelevant in practice.
//
// Not safe for concurrent use: DB holds its mutex across every call.
type entryCache struct {
	capacity int
	ll       *list.List // front = most recently used
	items    map[EntryKey]*list.Element
}

type cacheItem struct {
	key   EntryKey
	entry Entry
}

func newEntryCache(capacity int) *entryCache {
	if capacity < 1 {
		capacity = 1
	}
	return &entryCache{
		capacity: capacity,
		ll:       list.New(),
		items:    make(map[EntryKey]*list.Element, capacity),
	}
}

func (c *entryCache) get(k EntryKey) (Entry, bool) {
	el, ok := c.items[k]
	if !ok {
		return Entry{}, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*cacheItem).entry, true
}

func (c *entryCache) put(e Entry) {
	if el, ok := c.items[e.EntryKey]; ok {
		el.Value.(*cacheItem).entry = e
		c.ll.MoveToFront(el)
		return
	}
	c.items[e.EntryKey] = c.ll.PushFront(&cacheItem{key: e.EntryKey, entry: e})
	if c.ll.Len() > c.capacity {
		c.evictOldest()
	}
}

func (c *entryCache) drop(k EntryKey) {
	if el, ok := c.items[k]; ok {
		c.ll.Remove(el)
		delete(c.items, k)
	}
}

// dropProject invalidates every cached entry for a project. Called when a project is
// deleted, so a stale cached row can never resurrect content the catalog has dropped.
func (c *entryCache) dropProject(project string) {
	for k, el := range c.items {
		if k.Project == project {
			c.ll.Remove(el)
			delete(c.items, k)
		}
	}
}

func (c *entryCache) evictOldest() {
	el := c.ll.Back()
	if el == nil {
		return
	}
	c.ll.Remove(el)
	delete(c.items, el.Value.(*cacheItem).key)
}

func (c *entryCache) len() int { return c.ll.Len() }
