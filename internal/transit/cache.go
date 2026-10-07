package transit

import (
	"sync"
	"time"
)

// cache keeps values for a while (and at most max of them; the oldest go first when it is full).
type cache[V any] struct {
	ttl time.Duration
	max int
	now func() time.Time

	mu sync.Mutex
	m  map[string]entry[V]
}

type entry[V any] struct {
	v       V
	expires time.Time
}

func newCache[V any](ttl time.Duration, max int, now func() time.Time) *cache[V] {
	if now == nil {
		now = time.Now
	}
	return &cache[V]{ttl: ttl, max: max, now: now, m: map[string]entry[V]{}}
}

func (c *cache[V]) get(k string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || !c.now().Before(e.expires) {
		delete(c.m, k)
		var zero V
		return zero, false
	}
	return e.v, true
}

func (c *cache[V]) put(k string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.m) >= c.max {
		var oldest string
		var first time.Time
		for key, e := range c.m {
			if !now.Before(e.expires) {
				delete(c.m, key)
				continue
			}
			if oldest == "" || e.expires.Before(first) {
				oldest, first = key, e.expires
			}
		}
		if len(c.m) >= c.max {
			delete(c.m, oldest)
		}
	}
	c.m[k] = entry[V]{v: v, expires: now.Add(c.ttl)}
}
