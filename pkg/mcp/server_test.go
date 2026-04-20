package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// setupMCPProject creates a temp Go project with the given source files
// and returns the directory path.
func setupMCPProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testpkg\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func makeRequest(args map[string]any) mcpsdk.CallToolRequest {
	return mcpsdk.CallToolRequest{
		Params: struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments,omitempty"`
			Meta      *mcpsdk.Meta   `json:"_meta,omitempty"`
		}{
			Arguments: args,
		},
	}
}

func resultText(t *testing.T, result *mcpsdk.CallToolResult) string {
	t.Helper()
	if result.IsError {
		for _, c := range result.Content {
			if tc, ok := c.(mcpsdk.TextContent); ok {
				t.Fatalf("tool returned error: %s", tc.Text)
			}
		}
	}
	for _, c := range result.Content {
		if tc, ok := c.(mcpsdk.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatal("no text content in result")
	return ""
}

func resultJSON(t *testing.T, result *mcpsdk.CallToolResult) map[string]any {
	t.Helper()
	text := resultText(t, result)
	var data map[string]any
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}
	return data
}

// --------------------------------------------------------------------------
// find_pattern
// --------------------------------------------------------------------------

func TestMCP_FindPattern(t *testing.T) {
	dir := setupMCPProject(t, map[string]string{
		"code.go": `package testpkg

import "io"

func example(c io.Closer) {
	c.Close()
}
`,
	})

	srv, err := NewServer()
	if err != nil {
		t.Fatal(err)
	}

	req := makeRequest(map[string]any{
		"pattern": "$x.Close()",
		"path":    dir,
	})

	result, err := srv.handleFindPattern(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	data := resultJSON(t, result)
	total := data["total_matches"].(float64)
	if total == 0 {
		t.Error("expected at least one match for $x.Close()")
	}
}

func TestMCP_FindPattern_NoMatch(t *testing.T) {
	dir := setupMCPProject(t, map[string]string{
		"code.go": `package testpkg

func example() {
	x := 1
	_ = x
}
`,
	})

	srv, _ := NewServer()
	req := makeRequest(map[string]any{
		"pattern": "$x.FooBar()",
		"path":    dir,
	})

	result, err := srv.handleFindPattern(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	data := resultJSON(t, result)
	total := data["total_matches"].(float64)
	if total != 0 {
		t.Errorf("expected 0 matches, got %v", total)
	}
}

// --------------------------------------------------------------------------
// find_type_usage
// --------------------------------------------------------------------------

func TestMCP_FindTypeUsage(t *testing.T) {
	dir := setupMCPProject(t, map[string]string{
		"code.go": `package testpkg

type Config struct{ Name string }

func use() {
	c := Config{Name: "test"}
	_ = c
}
`,
	})

	srv, _ := NewServer()
	req := makeRequest(map[string]any{
		"type_name": "Config",
		"path":      dir,
	})

	result, err := srv.handleFindTypeUsage(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	data := resultJSON(t, result)
	total := data["total_usages"].(float64)
	if total == 0 {
		t.Error("expected at least one usage of Config")
	}
}

// --------------------------------------------------------------------------
// find_resource_leak
// --------------------------------------------------------------------------

func TestMCP_FindResourceLeak(t *testing.T) {
	dir := setupMCPProject(t, map[string]string{
		"code.go": `package testpkg

import "os"

func leaky() {
	f, _ := os.Open("test.txt")
	_ = f
}
`,
	})

	srv, _ := NewServer()
	req := makeRequest(map[string]any{
		"acquire_pattern": "$f, $_ := os.Open($path)",
		"release_method":  "Close",
		"path":            dir,
	})

	result, err := srv.handleFindResourceLeak(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	data := resultJSON(t, result)
	total := data["total_findings"].(float64)
	if total == 0 {
		t.Error("expected at least one resource leak finding")
	}
}

// --------------------------------------------------------------------------
// find_unchecked_error
// --------------------------------------------------------------------------

func TestMCP_FindUncheckedError(t *testing.T) {
	dir := setupMCPProject(t, map[string]string{
		"code.go": `package testpkg

import "os"

func bad() {
	f, _ := os.Open("test.txt")
	f.Close()
}
`,
	})

	srv, _ := NewServer()
	req := makeRequest(map[string]any{
		"path": dir,
	})

	result, err := srv.handleFindUncheckedError(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	data := resultJSON(t, result)
	total := data["total_findings"].(float64)
	if total == 0 {
		t.Error("expected at least one unchecked error finding")
	}
}

// --------------------------------------------------------------------------
// find_lock_violation
// --------------------------------------------------------------------------

func TestMCP_FindLockViolation(t *testing.T) {
	dir := setupMCPProject(t, map[string]string{
		"code.go": `package testpkg

import "sync"

func noUnlock() {
	var mu sync.Mutex
	mu.Lock()
}
`,
	})

	srv, _ := NewServer()
	req := makeRequest(map[string]any{
		"path": dir,
	})

	result, err := srv.handleFindLockViolation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	data := resultJSON(t, result)
	total := data["total_findings"].(float64)
	if total == 0 {
		t.Error("expected at least one lock violation finding")
	}
}

// --------------------------------------------------------------------------
// apply_transform (preview)
// --------------------------------------------------------------------------

func TestMCP_ApplyTransform_Preview(t *testing.T) {
	dir := setupMCPProject(t, map[string]string{
		"code.go": `package testpkg

import "io"

func example(c io.Closer) {
	c.Close()
}
`,
	})

	srv, _ := NewServer()
	req := makeRequest(map[string]any{
		"match":   "$x.Close()",
		"replace": "$x.Release()",
		"path":    dir,
		"preview": true,
	})

	result, err := srv.handleApplyTransform(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	data := resultJSON(t, result)
	if data["preview"] != true {
		t.Error("expected preview=true")
	}
	total := data["total_changes"].(float64)
	if total == 0 {
		t.Error("expected at least one change in preview")
	}

	// Verify file was NOT modified (preview mode).
	content, _ := os.ReadFile(filepath.Join(dir, "code.go"))
	if strings.Contains(string(content), "Release") {
		t.Error("file should not be modified in preview mode")
	}
}

// --------------------------------------------------------------------------
// list_equivalences
// --------------------------------------------------------------------------

func TestMCP_ListEquivalences(t *testing.T) {
	srv, _ := NewServer()
	req := makeRequest(map[string]any{
		"category": "all",
	})

	result, err := srv.handleListEquivalences(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	data := resultJSON(t, result)
	total := data["total"].(float64)
	if total == 0 {
		t.Error("expected at least one equivalence")
	}
}

// --------------------------------------------------------------------------
// check_path_property
// --------------------------------------------------------------------------

func TestMCP_CheckPathProperty_AlwaysBefore(t *testing.T) {
	dir := setupMCPProject(t, map[string]string{
		"code.go": `package testpkg

func process() {
	doWork()
}

func validate() {}
func doWork()   {}
`,
	})

	srv, _ := NewServer()
	req := makeRequest(map[string]any{
		"from_pattern":   "doWork()",
		"target_pattern": "validate()",
		"property":       "always_before",
		"path":           dir,
	})

	result, err := srv.handleCheckPathProperty(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	data := resultJSON(t, result)
	// validate() is NOT called before doWork() → should report violation.
	total := data["total_findings"].(float64)
	if total == 0 {
		t.Error("expected violation: validate() not called before doWork()")
	}
}

// --------------------------------------------------------------------------
// Error handling
// --------------------------------------------------------------------------

func TestMCP_InvalidPattern(t *testing.T) {
	srv, _ := NewServer()
	req := makeRequest(map[string]any{
		"pattern": "$",
		"path":    ".",
	})

	result, err := srv.handleFindPattern(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	if !result.IsError {
		t.Error("expected error result for invalid pattern")
	}
}
