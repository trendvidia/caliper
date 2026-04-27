package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// defaultCacheMaxBytes caps disk usage for $TMPDIR/caliper-cache/.
// Override via env var CALIPER_CACHE_MAX_BYTES (set to 0 to disable
// eviction entirely).
const defaultCacheMaxBytes int64 = 100 * 1024 * 1024 // 100 MB

// resultCache persists tool-call results to disk, keyed on a hash of
// (tool name, analysis path, arguments, source-file fingerprint). The
// fingerprint changes whenever a .go file under the analysis path is
// modified, so cache hits only happen for repeat invocations on
// unchanged source.
//
// Why this and not engine caching: persisting *types.Package data
// across processes is essentially infeasible — go/types maps contain
// unexported state and circular references that don't gob/json
// cleanly. Result caching is the practical alternative that achieves
// the same UX win for the dominant case: a user reconnects MCP, runs
// the same checks they ran in the previous batch, and gets immediate
// answers instead of paying the multi-second packages.Load cost
// every time the client tears down stdio.
type resultCache struct {
	dir      string
	maxBytes int64 // 0 = no eviction

	// fpCache memoizes fingerprints within one process so a fan-out
	// of N tool calls on the same path walks the source tree once.
	mu      sync.Mutex
	fpCache map[string]string // absPath → fingerprint

	// evicting ensures only one background eviction runs at a time —
	// repeated rapid Puts don't spawn parallel scanners of the cache
	// dir. Set to 1 while a goroutine is running, 0 otherwise.
	evicting atomic.Int32
}

func newResultCache(dir string) *resultCache {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "caliper-cache")
	}
	maxBytes := defaultCacheMaxBytes
	if v := os.Getenv("CALIPER_CACHE_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			maxBytes = n
		}
	}
	return &resultCache{
		dir:      dir,
		maxBytes: maxBytes,
		fpCache:  map[string]string{},
	}
}

// keyFor computes a stable cache key. argsJSON is canonical because
// Go's json.Marshal sorts map keys deterministically.
func (c *resultCache) keyFor(toolName, absPath string, args map[string]any) (string, error) {
	fp, err := c.fingerprint(absPath)
	if err != nil {
		return "", err
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00", toolName, absPath)
	h.Write(argsJSON)
	h.Write([]byte{0})
	h.Write([]byte(fp))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fingerprint walks absPath and returns a hash over (relpath, mtime,
// size) for every .go file. Skips vendor/, testdata/, and .git/ to
// keep the walk cheap on large projects (~35ms on Go stdlib).
func (c *resultCache) fingerprint(absPath string) (string, error) {
	c.mu.Lock()
	if fp, ok := c.fpCache[absPath]; ok {
		c.mu.Unlock()
		return fp, nil
	}
	c.mu.Unlock()

	type fileInfo struct {
		rel  string
		mod  int64
		size int64
	}
	var files []fileInfo
	err := filepath.WalkDir(absPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort; ignore unreadable subtrees
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "testdata", ".git", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(absPath, p)
		if err != nil {
			rel = p
		}
		files = append(files, fileInfo{rel: rel, mod: info.ModTime().UnixNano(), size: info.Size()})
		return nil
	})
	if err != nil {
		return "", err
	}

	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\t%d\t%d\n", f.rel, f.mod, f.size)
	}
	fp := hex.EncodeToString(h.Sum(nil))

	c.mu.Lock()
	c.fpCache[absPath] = fp
	c.mu.Unlock()
	return fp, nil
}

// Get returns the cached result for key, or nil if not cached or
// unreadable. Errors are swallowed — the cache is best-effort.
func (c *resultCache) Get(key string) *mcpsdk.CallToolResult {
	if key == "" {
		return nil
	}
	data, err := os.ReadFile(c.path(key))
	if err != nil {
		return nil
	}
	var result mcpsdk.CallToolResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil
	}
	return &result
}

// Put stores result under key. Errors are swallowed — the cache is
// best-effort and a write failure shouldn't surface to the user.
//
// After a successful write, kicks off a fire-and-forget eviction
// pass if maxBytes is set and another eviction isn't already running.
func (c *resultCache) Put(key string, result *mcpsdk.CallToolResult) {
	if key == "" || result == nil {
		return
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return
	}
	data, err := json.Marshal(result)
	if err != nil {
		return
	}
	if err := os.WriteFile(c.path(key), data, 0o644); err != nil {
		return
	}
	if c.maxBytes > 0 && c.evicting.CompareAndSwap(0, 1) {
		go func() {
			defer c.evicting.Store(0)
			c.evict()
		}()
	}
}

// evict deletes the oldest entries (by mtime) until the directory
// total falls below maxBytes. Best-effort: errors during scan or
// removal are silently ignored.
func (c *resultCache) evict() {
	type entry struct {
		path string
		size int64
		mod  int64
	}
	var entries []entry
	var total int64

	_ = filepath.WalkDir(c.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		entries = append(entries, entry{path: p, size: info.Size(), mod: info.ModTime().UnixNano()})
		total += info.Size()
		return nil
	})

	if total <= c.maxBytes {
		return
	}

	// Oldest first.
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod < entries[j].mod })

	for _, e := range entries {
		if total <= c.maxBytes {
			break
		}
		if err := os.Remove(e.path); err == nil {
			total -= e.size
		}
	}
}

func (c *resultCache) path(key string) string {
	return filepath.Join(c.dir, key+".json")
}
