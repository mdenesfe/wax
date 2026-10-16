package client

import (
	"sync"
	"time"
)

// maxCacheEntries bounds a ttlCache. When it is reached, expired entries
// are swept (O(n), amortized over the puts that filled the cache); if all
// are still fresh the cache starts over, which only costs refetches.
const maxCacheEntries = 10000

// ttlCache is a bounded map whose entries expire after ttl.
type ttlCache[K comparable, V any] struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[K]ttlEntry[V]
}

type ttlEntry[V any] struct {
	value   V
	fetched time.Time
}

func newTTLCache[K comparable, V any](ttl time.Duration) ttlCache[K, V] {
	return ttlCache[K, V]{ttl: ttl}
}

func (c *ttlCache[K, V]) get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok || time.Since(e.fetched) > c.ttl {
		var zero V
		return zero, false
	}
	return e.value, true
}

func (c *ttlCache[K, V]) put(k K, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[K]ttlEntry[V])
	}
	if len(c.entries) >= maxCacheEntries {
		for key, e := range c.entries {
			if time.Since(e.fetched) > c.ttl {
				delete(c.entries, key)
			}
		}
		if len(c.entries) >= maxCacheEntries {
			clear(c.entries)
		}
	}
	c.entries[k] = ttlEntry[V]{value: v, fetched: time.Now()}
}

func (c *ttlCache[K, V]) invalidate(k K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, k)
}
