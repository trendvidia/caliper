package mcp

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trendvidia/caliper/pkg/analysis"
)

func TestEngineCache_PutAndGet(t *testing.T) {
	c := newEngineCache(4)

	// Miss on empty cache.
	if eng := c.get("/foo"); eng != nil {
		t.Error("expected nil on empty cache")
	}

	hits, misses := c.stats()
	if hits != 0 || misses != 1 {
		t.Errorf("expected 0 hits, 1 miss; got %d, %d", hits, misses)
	}

	// Insert a mock engine (use nil since we just test the cache structure).
	c.put("/foo", (*analysis.Engine)(nil), nil)

	if c.size() != 1 {
		t.Errorf("expected size 1, got %d", c.size())
	}

	// Hit on exact path.
	eng := c.get("/foo")
	if eng != nil {
		// We stored nil, so getting nil back is correct.
	}
	hits, _ = c.stats()
	if hits != 1 {
		t.Errorf("expected 1 hit, got %d", hits)
	}
}

func TestEngineCache_SubPathMatch(t *testing.T) {
	c := newEngineCache(4)
	c.put("/project/root", (*analysis.Engine)(nil), nil)

	// Sub-path should hit the parent entry.
	c.get("/project/root/pkg/foo")
	hits, _ := c.stats()
	if hits != 1 {
		t.Errorf("expected sub-path hit, got %d hits", hits)
	}

	// Non-sub-path should miss.
	c.get("/other/path")
	_, misses := c.stats()
	if misses != 1 {
		t.Errorf("expected 1 miss for non-sub-path, got %d", misses)
	}
}

func TestEngineCache_LRUEviction(t *testing.T) {
	c := newEngineCache(2)

	c.put("/a", (*analysis.Engine)(nil), nil)
	c.put("/b", (*analysis.Engine)(nil), nil)
	if c.size() != 2 {
		t.Fatalf("expected size 2, got %d", c.size())
	}

	// Adding a third should evict /a (LRU).
	c.put("/c", (*analysis.Engine)(nil), nil)
	if c.size() != 2 {
		t.Errorf("expected size 2 after eviction, got %d", c.size())
	}

	// /a should be gone (miss), /b and /c should remain.
	c.get("/a")
	_, misses := c.stats()
	if misses != 1 {
		t.Errorf("expected miss for evicted /a, got %d misses", misses)
	}
}

func TestEngineCache_LRUAccess(t *testing.T) {
	c := newEngineCache(2)

	c.put("/a", (*analysis.Engine)(nil), nil)
	c.put("/b", (*analysis.Engine)(nil), nil)

	// Access /a to make it most recently used.
	c.get("/a")

	// Now adding /c should evict /b (the LRU), not /a.
	c.put("/c", (*analysis.Engine)(nil), nil)

	// /a should still be cached (hit).
	c.get("/a")
	hits, _ := c.stats()
	if hits != 2 { // first get + second get
		t.Errorf("expected /a to survive eviction, got %d hits", hits)
	}
}

func TestEngineCache_Invalidate(t *testing.T) {
	c := newEngineCache(4)
	c.put("/foo", (*analysis.Engine)(nil), nil)

	c.invalidate("/foo")

	if c.size() != 0 {
		t.Errorf("expected empty cache after invalidation, got size %d", c.size())
	}

	// Invalidating nonexistent key should be a no-op.
	c.invalidate("/bar")
}

func TestEngineCache_LockSerializesColdLoad(t *testing.T) {
	// Regression: before the fix, lock() returned a no-op when no entry
	// was cached for the path. A fan-out of N parallel tool calls on a
	// cold path would all enter the load critical section concurrently,
	// triggering N concurrent packages.Load invocations and crashing the
	// MCP server. The lock must serialize first-load too.
	c := newEngineCache(4)

	const goroutines = 16
	var (
		inFlight int32
		maxSeen  int32
		wg       sync.WaitGroup
	)
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			unlock := c.lock("/cold/path")
			defer unlock()

			n := atomic.AddInt32(&inFlight, 1)
			defer atomic.AddInt32(&inFlight, -1)

			// Track the high-water mark of concurrent holders.
			for {
				m := atomic.LoadInt32(&maxSeen)
				if n <= m || atomic.CompareAndSwapInt32(&maxSeen, m, n) {
					break
				}
			}

			// Simulate the heavy packages.Load work the lock is meant
			// to serialize.
			time.Sleep(5 * time.Millisecond)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxSeen); got != 1 {
		t.Errorf("expected at most 1 goroutine in critical section at a time, saw %d", got)
	}
}

func TestEngineCache_LockReusesParentMutex(t *testing.T) {
	// Sub-path requests must share the parent path's lock so that loads
	// of /foo and /foo/bar serialize against the same engine creation.
	c := newEngineCache(4)
	c.put("/foo", (*analysis.Engine)(nil), nil)

	const goroutines = 8
	var (
		inFlight int32
		maxSeen  int32
		wg       sync.WaitGroup
	)
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		path := "/foo"
		if i%2 == 1 {
			path = "/foo/sub"
		}
		go func(p string) {
			defer wg.Done()
			unlock := c.lock(p)
			defer unlock()

			n := atomic.AddInt32(&inFlight, 1)
			defer atomic.AddInt32(&inFlight, -1)
			for {
				m := atomic.LoadInt32(&maxSeen)
				if n <= m || atomic.CompareAndSwapInt32(&maxSeen, m, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
		}(path)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxSeen); got != 1 {
		t.Errorf("expected /foo and /foo/sub to share a lock, but %d ran concurrently", got)
	}
}

func TestIsSubPath(t *testing.T) {
	tests := []struct {
		parent, child string
		want          bool
	}{
		{"/foo", "/foo/bar", true},
		{"/foo", "/foo/bar/baz", true},
		{"/foo", "/foo", false}, // exact match, not sub-path
		{"/foo", "/foobar", false},
		{"/foo", "/bar", false},
		{"/foo/bar", "/foo", false},
	}

	for _, tt := range tests {
		got := isSubPath(tt.parent, tt.child)
		if got != tt.want {
			t.Errorf("isSubPath(%q, %q) = %v, want %v", tt.parent, tt.child, got, tt.want)
		}
	}
}
