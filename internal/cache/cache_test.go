package cache

import (
	"strconv"
	"testing"
	"time"
)

func entry(body string) *Entry {
	return &Entry{Status: 200, Body: []byte(body), StoredAt: time.Now(), FreshFor: time.Minute}
}

func TestGetPut(t *testing.T) {
	c := New(1 << 20)
	if _, ok := c.Get("missing"); ok {
		t.Error("empty cache returned a hit")
	}
	c.Put("k", entry("value"))
	got, ok := c.Get("k")
	if !ok {
		t.Fatal("stored entry was not returned")
	}
	if string(got.Body) != "value" {
		t.Errorf("body = %q, want \"value\"", got.Body)
	}
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	c := New(1024)
	for i := range 20 {
		c.Put("k"+strconv.Itoa(i), entry("0123456789"))
	}
	st := c.Stats()
	if st.Bytes > 1024 {
		t.Errorf("cache holds %d bytes, over its 1024 budget", st.Bytes)
	}
	if st.Evictions == 0 {
		t.Error("expected evictions once the budget was exceeded")
	}
	if _, ok := c.Get("k19"); !ok {
		t.Error("the most recently used entry was evicted")
	}
}

func TestOversizedEntryIsNotStored(t *testing.T) {
	c := New(256)
	c.Put("big", &Entry{Status: 200, Body: make([]byte, 1024), StoredAt: time.Now(), FreshFor: time.Minute})
	if _, ok := c.Get("big"); ok {
		t.Error("an entry larger than the whole budget should not be stored")
	}
}

func TestFreshnessWindows(t *testing.T) {
	now := time.Now()
	e := &Entry{StoredAt: now.Add(-30 * time.Second), FreshFor: 10 * time.Second, StaleFor: time.Minute}
	if e.Fresh(now) {
		t.Error("a 30s-old entry with a 10s freshness window is not fresh")
	}
	if !e.Usable(now) {
		t.Error("the entry is within its stale window and should be usable")
	}

	old := &Entry{StoredAt: now.Add(-time.Hour), FreshFor: 10 * time.Second, StaleFor: time.Minute}
	if old.Usable(now) {
		t.Error("an entry past its stale window must not be served")
	}
}
