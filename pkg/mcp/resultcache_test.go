package mcp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

func TestResultCache_PutAndGet(t *testing.T) {
	c := newResultCache(t.TempDir())
	want := &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{
			mcpsdk.TextContent{Type: "text", Text: "hello"},
		},
	}
	c.Put("k1", want)

	got := c.Get("k1")
	if got == nil {
		t.Fatal("expected cached result, got nil")
	}
	if len(got.Content) != 1 {
		t.Fatalf("expected 1 content item, got %d", len(got.Content))
	}
	tc, ok := got.Content[0].(mcpsdk.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", got.Content[0])
	}
	if tc.Text != "hello" {
		t.Errorf("expected text=hello, got %q", tc.Text)
	}
}

func TestResultCache_Miss(t *testing.T) {
	c := newResultCache(t.TempDir())
	if got := c.Get("nonexistent"); got != nil {
		t.Errorf("expected nil on cache miss, got %v", got)
	}
}

func TestResultCache_FingerprintInvariantUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newResultCache(t.TempDir())

	fp1, err := c.fingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 == "" {
		t.Fatal("fingerprint must be non-empty for non-empty dir")
	}

	// Reset the per-process memo so we recompute from disk.
	c.fpCache = map[string]string{}

	fp2, err := c.fingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint must be stable across calls; got %s then %s", fp1, fp2)
	}
}

func TestResultCache_FingerprintChangesOnEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newResultCache(t.TempDir())

	fp1, err := c.fingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Mutate file content + bump mtime explicitly so the change is
	// observable even on filesystems with second-grained mtime.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("package x\n\nvar y = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	c.fpCache = map[string]string{} // bypass per-process memo
	fp2, err := c.fingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 == fp2 {
		t.Errorf("fingerprint must change when source files change; got %s both times", fp1)
	}
}

func TestResultCache_KeyVariesByArgs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newResultCache(t.TempDir())

	k1, err := c.keyFor("find_unchecked_error", dir, map[string]any{"path": dir, "include_defers": false})
	if err != nil {
		t.Fatal(err)
	}
	k2, err := c.keyFor("find_unchecked_error", dir, map[string]any{"path": dir, "include_defers": true})
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Error("cache key must differ when args differ")
	}
}

func TestResultCache_KeyVariesByTool(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newResultCache(t.TempDir())

	k1, err := c.keyFor("find_unchecked_error", dir, map[string]any{"path": dir})
	if err != nil {
		t.Fatal(err)
	}
	k2, err := c.keyFor("find_resource_leak", dir, map[string]any{"path": dir})
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Error("cache key must differ when tool name differs")
	}
}
