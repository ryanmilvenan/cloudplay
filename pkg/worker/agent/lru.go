package agent

import (
	"container/list"
	"sync"
)

// queryEmbedCache is a tiny bounded LRU keyed on the query string.
// Phase-3's embedder call takes ~50-150 ms; the agent retrieves on
// every turn, and repeat queries ("FIFA", "mario") during an active
// conversation are the common case. Caching the vector eliminates the
// HTTP round-trip entirely for those, cutting perceived latency.
//
// Size is small (32) — the cache is a warm-path optimization, not a
// persistence layer. Items expire only by eviction, never by time.
type queryEmbedCache struct {
	mu    sync.Mutex
	cap   int
	order *list.List               // front = most recently used
	items map[string]*list.Element // query → *list.Element (value = *embedCacheEntry)
}

type embedCacheEntry struct {
	query  string
	vector []float32
}

func newQueryEmbedCache(capacity int) *queryEmbedCache {
	if capacity <= 0 {
		capacity = 32
	}
	return &queryEmbedCache{
		cap:   capacity,
		order: list.New(),
		items: make(map[string]*list.Element),
	}
}

// Get returns the cached vector and true if present, moving the entry
// to the front as the new MRU.
func (c *queryEmbedCache) Get(query string) ([]float32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[query]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*embedCacheEntry).vector, true
}

// Put stores (query, vector) as MRU, evicting the LRU if the cache
// is full. Copies the vector so later mutation on the caller's slice
// doesn't invalidate the cache.
func (c *queryEmbedCache) Put(query string, vector []float32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[query]; ok {
		c.order.MoveToFront(el)
		el.Value.(*embedCacheEntry).vector = append([]float32(nil), vector...)
		return
	}
	v := append([]float32(nil), vector...)
	el := c.order.PushFront(&embedCacheEntry{query: query, vector: v})
	c.items[query] = el
	if c.order.Len() > c.cap {
		back := c.order.Back()
		if back != nil {
			c.order.Remove(back)
			delete(c.items, back.Value.(*embedCacheEntry).query)
		}
	}
}
