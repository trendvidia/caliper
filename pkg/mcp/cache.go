package mcp

import (
	"container/list"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/trendvidia/caliper/pkg/analysis"
	"github.com/trendvidia/caliper/pkg/analysis/templates"
)

const defaultMaxCacheEntries = 3

// engineCache is an LRU cache of analysis engines keyed by absolute path.
// It supports sub-path lookups: if /foo is cached and /foo/bar is requested,
// the /foo engine is reused since it loads ./... which covers sub-packages.
type engineCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List // front = most recently used
	maxSize int

	// pathLocks serializes both first-load AND subsequent analysis on the
	// same path. Distinct from per-cacheEntry locks because the lock must
	// exist BEFORE any engine is cached — otherwise a fan-out of N
	// parallel tool calls on a cold path triggers N concurrent
	// packages.Load invocations, which hammer the OS (file descriptors,
	// subprocess spawns, memory) and can panic in goroutines that aren't
	// covered by the stdio handler's recovery, killing the process.
	pathLocks map[string]*sync.Mutex

	// Stats
	hits   int64
	misses int64
}

type cacheEntry struct {
	path       string
	engine     *analysis.Engine
	infallible *templates.InfallibleState // engine-scoped project-local config; nil = none
}

func newEngineCache(maxSize int) *engineCache {
	if maxSize <= 0 {
		maxSize = defaultMaxCacheEntries
	}
	return &engineCache{
		entries:   make(map[string]*list.Element),
		order:     list.New(),
		maxSize:   maxSize,
		pathLocks: make(map[string]*sync.Mutex),
	}
}

// get returns a cached engine for the given absolute path, or nil.
// It checks for exact matches first, then tries to find a parent
// directory that covers the requested path.
func (c *engineCache) get(absPath string) *analysis.Engine {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Exact match.
	if elem, ok := c.entries[absPath]; ok {
		c.order.MoveToFront(elem)
		c.hits++
		return elem.Value.(*cacheEntry).engine
	}

	// Sub-path match: check if any cached engine's path is a parent.
	for _, elem := range c.entries {
		entry := elem.Value.(*cacheEntry)
		if isSubPath(entry.path, absPath) {
			c.order.MoveToFront(elem)
			c.hits++
			return entry.engine
		}
	}

	c.misses++
	return nil
}

// lock acquires the per-path mutex, serializing both first-load and
// subsequent analysis for the given path. Returns an unlock function.
//
// On a cold path (no cached engine yet) this still returns a real
// mutex — that's the critical fix for the parallel-fan-out crash. If
// any cached engine covers absPath as a parent directory, that parent
// path's mutex is reused so sub-path requests serialize against the
// same engine.
func (c *engineCache) lock(absPath string) func() {
	c.mu.Lock()
	key := absPath
	if _, ok := c.entries[absPath]; !ok {
		for _, elem := range c.entries {
			e := elem.Value.(*cacheEntry)
			if isSubPath(e.path, absPath) {
				key = e.path
				break
			}
		}
	}
	pl, ok := c.pathLocks[key]
	if !ok {
		pl = &sync.Mutex{}
		c.pathLocks[key] = pl
	}
	c.mu.Unlock()

	pl.Lock()
	return pl.Unlock
}

// put adds an engine to the cache, evicting the least recently used
// entry if the cache is full. If state is non-nil, it is stored alongside
// the engine and will be returned by infallibleFor for subsequent lookups
// at the same path.
func (c *engineCache) put(absPath string, engine *analysis.Engine, state *templates.InfallibleState) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Update existing entry.
	if elem, ok := c.entries[absPath]; ok {
		entry := elem.Value.(*cacheEntry)
		entry.engine = engine
		entry.infallible = state
		c.order.MoveToFront(elem)
		return
	}

	// Evict LRU if at capacity.
	if c.order.Len() >= c.maxSize {
		oldest := c.order.Back()
		if oldest != nil {
			c.order.Remove(oldest)
			delete(c.entries, oldest.Value.(*cacheEntry).path)
			// Nudge GC to reclaim the evicted engine's AST/type data.
			runtime.GC()
		}
	}

	entry := &cacheEntry{path: absPath, engine: engine, infallible: state}
	elem := c.order.PushFront(entry)
	c.entries[absPath] = elem
}

// infallibleFor returns the cached InfallibleState for the engine that
// covers absPath (exact or sub-path match), or nil if none is cached.
// Read under the same mutex as get so a concurrent put doesn't return a
// torn engine/state pair.
func (c *engineCache) infallibleFor(absPath string) *templates.InfallibleState {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.entries[absPath]; ok {
		return elem.Value.(*cacheEntry).infallible
	}
	for _, elem := range c.entries {
		entry := elem.Value.(*cacheEntry)
		if isSubPath(entry.path, absPath) {
			return entry.infallible
		}
	}
	return nil
}

// invalidate removes the engine for the given path from the cache.
func (c *engineCache) invalidate(absPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.entries[absPath]; ok {
		c.order.Remove(elem)
		delete(c.entries, absPath)
	}
}

// stats returns cache hit and miss counts.
func (c *engineCache) stats() (hits, misses int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

// size returns the number of cached entries.
func (c *engineCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// isSubPath reports whether child is under the parent directory.
func isSubPath(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if parent == child {
		return false // exact match handled separately
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}
