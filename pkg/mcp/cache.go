package mcp

import (
	"container/list"
	"path/filepath"
	"strings"
	"sync"

	"github.com/trendvidia/caliper/pkg/analysis"
)

const defaultMaxCacheEntries = 8

// engineCache is an LRU cache of analysis engines keyed by absolute path.
// It supports sub-path lookups: if /foo is cached and /foo/bar is requested,
// the /foo engine is reused since it loads ./... which covers sub-packages.
type engineCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List // front = most recently used
	maxSize int

	// Stats
	hits   int64
	misses int64
}

type cacheEntry struct {
	path   string
	engine *analysis.Engine
}

func newEngineCache(maxSize int) *engineCache {
	if maxSize <= 0 {
		maxSize = defaultMaxCacheEntries
	}
	return &engineCache{
		entries: make(map[string]*list.Element),
		order:   list.New(),
		maxSize: maxSize,
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

// put adds an engine to the cache, evicting the least recently used
// entry if the cache is full.
func (c *engineCache) put(absPath string, engine *analysis.Engine) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Update existing entry.
	if elem, ok := c.entries[absPath]; ok {
		elem.Value.(*cacheEntry).engine = engine
		c.order.MoveToFront(elem)
		return
	}

	// Evict LRU if at capacity.
	if c.order.Len() >= c.maxSize {
		oldest := c.order.Back()
		if oldest != nil {
			c.order.Remove(oldest)
			delete(c.entries, oldest.Value.(*cacheEntry).path)
		}
	}

	entry := &cacheEntry{path: absPath, engine: engine}
	elem := c.order.PushFront(entry)
	c.entries[absPath] = elem
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
