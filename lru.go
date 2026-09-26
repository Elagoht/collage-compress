package compress

import (
	"container/list"
	"sync"
)

// lru keeps compressed bodies under (ETag, encoding), bounded by the bytes they
// hold, and drops the least recently served first.
type lru struct {
	mu    sync.Mutex
	limit int
	size  int
	order *list.List // front is the most recently used
	items map[string]*list.Element
}

type lruEntry struct {
	key  string
	body []byte
}

func newLRU(limit int) *lru {
	return &lru{limit: limit, order: list.New(), items: make(map[string]*list.Element)}
}

// maxEntry is the largest body kept. One response filling the cache would evict
// every page for the sake of a single large file.
func (c *lru) maxEntry() int { return c.limit / 8 }

func (c *lru) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*lruEntry).body, true
}

// put keeps a copy of body: the caller's buffer is its own to reuse.
func (c *lru) put(key string, body []byte) {
	if len(body) > c.maxEntry() {
		return
	}
	kept := make([]byte, len(body))
	copy(kept, body)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.size -= len(el.Value.(*lruEntry).body)
		el.Value.(*lruEntry).body = kept
		c.size += len(kept)
		c.order.MoveToFront(el)
	} else {
		c.items[key] = c.order.PushFront(&lruEntry{key: key, body: kept})
		c.size += len(kept)
	}
	for c.size > c.limit {
		oldest := c.order.Back()
		entry := oldest.Value.(*lruEntry)
		c.order.Remove(oldest)
		delete(c.items, entry.key)
		c.size -= len(entry.body)
	}
}
