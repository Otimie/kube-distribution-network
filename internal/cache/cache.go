// Package cache is the proxy's in-process response cache.
//
// This is the second cache in the chain, behind the CDN. It matters because the
// CDN protects the origin only for requests it has already seen: cold URLs, and
// each CDN location missing independently, all land here.
package cache

import (
	"container/list"
	"sync"
	"time"
)

// Entry is a cached response.
type Entry struct {
	Status      int
	Body        []byte
	ContentType string

	// ETag is a strong validator over the body, for 304 replies.
	ETag string

	// ResourceVersion is what the API server reported, surfaced as a response
	// header for observability.
	ResourceVersion string

	StoredAt time.Time

	// FreshFor is how long the entry can be served without contacting the API
	// server.
	FreshFor time.Duration

	// StaleFor is how long past freshness the entry may still be served: in the
	// background while a refresh runs, or immediately if the API server is
	// failing. Serving data a minute old beats propagating an outage to every
	// client at once.
	StaleFor time.Duration
}

// Age returns how long ago the entry was stored.
func (e *Entry) Age(now time.Time) time.Duration { return now.Sub(e.StoredAt) }

// Fresh reports whether the entry can be served as-is.
func (e *Entry) Fresh(now time.Time) bool { return e.Age(now) < e.FreshFor }

// Usable reports whether the entry may be served while stale.
func (e *Entry) Usable(now time.Time) bool { return e.Age(now) < e.FreshFor+e.StaleFor }

func (e *Entry) size() int64 {
	return int64(len(e.Body) + len(e.ContentType) + len(e.ETag) + len(e.ResourceVersion) + 128)
}

type node struct {
	key   string
	entry *Entry
}

// Cache is a size-bounded LRU cache, safe for concurrent use.
type Cache struct {
	mu       sync.Mutex
	maxBytes int64
	bytes    int64
	ll       *list.List // front is most recently used
	items    map[string]*list.Element

	hits, misses, evictions uint64
}

// New builds a Cache with the given total body budget.
func New(maxBytes int64) *Cache {
	return &Cache{
		maxBytes: maxBytes,
		ll:       list.New(),
		items:    map[string]*list.Element{},
	}
}

// Get returns the entry for key if present. It may be stale; the caller decides
// using Entry's predicates.
func (c *Cache) Get(key string) (*Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.misses++
		return nil, false
	}
	c.hits++
	c.ll.MoveToFront(el)
	return el.Value.(*node).entry, true
}

// Put stores an entry, evicting least-recently-used entries to stay in budget.
func (c *Cache) Put(key string, e *Entry) {
	sz := e.size()
	// A single response larger than the whole budget would evict everything
	// else, so it is served but not stored.
	if sz > c.maxBytes {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[key]; ok {
		n := el.Value.(*node)
		c.bytes += sz - n.entry.size()
		n.entry = e
		c.ll.MoveToFront(el)
	} else {
		c.items[key] = c.ll.PushFront(&node{key: key, entry: e})
		c.bytes += sz
	}

	for c.bytes > c.maxBytes {
		back := c.ll.Back()
		if back == nil {
			break
		}
		n := back.Value.(*node)
		c.ll.Remove(back)
		delete(c.items, n.key)
		c.bytes -= n.entry.size()
		c.evictions++
	}
}

// Stats is a snapshot of cache state.
type Stats struct {
	Entries   int    `json:"entries"`
	Bytes     int64  `json:"bytes"`
	Hits      uint64 `json:"hits"`
	Misses    uint64 `json:"misses"`
	Evictions uint64 `json:"evictions"`
}

// Stats returns a snapshot.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		Entries:   len(c.items),
		Bytes:     c.bytes,
		Hits:      c.hits,
		Misses:    c.misses,
		Evictions: c.evictions,
	}
}
