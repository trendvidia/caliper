// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package mcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/trendvidia/caliper/pkg/config"
	"github.com/trendvidia/caliper/pkg/iso"
)

// Server is the Caliper MCP server.
type Server struct {
	mcp     *mcpserver.MCPServer
	iso     *iso.Registry
	cache   *engineCache
	results *resultCache
	cfg     *config.Config
	// version is the release identifier passed to NewServer. Stamped
	// into SARIF output so consumers can pin findings to a build.
	version string
}

// NewServer creates and configures the MCP server with all tools.
// It loads .caliper.pxf from the current working directory (or parents).
//
// version is the release identifier exposed in the MCP initialize
// handshake. The caller (cmd/caliper-mcp) typically passes a value set
// at build time via -ldflags or derived from runtime/debug.BuildInfo.
func NewServer(version string) (*Server, error) {
	cfg, err := config.Load(".")
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	cacheSize := defaultMaxCacheEntries
	if cfg.MaxCacheEntries() > 0 {
		cacheSize = cfg.MaxCacheEntries()
	}

	if version == "" {
		version = "dev"
	}

	s := &Server{
		iso:     iso.NewRegistry(),
		cache:   newEngineCache(cacheSize),
		results: newResultCache(""), // default $TMPDIR/caliper-cache
		cfg:     cfg,
		version: version,
	}

	s.mcp = mcpserver.NewMCPServer(
		"Caliper",
		version,
		mcpserver.WithToolCapabilities(true),
	)

	s.registerTools()
	return s, nil
}

// Run starts the MCP server on stdio.
func (s *Server) Run(ctx context.Context) error {
	stdio := mcpserver.NewStdioServer(s.mcp)
	// Wrap stdin with a diagnostic reader that logs EOF and per-read
	// stats — useful for diagnosing "the MCP client closed the
	// connection" symptoms that look like silent disconnects.
	in := &diagReader{r: os.Stdin, started: time.Now()}
	defer in.logExit()
	// Wrap stdout with a mutex to prevent interleaved writes between
	// the main message handler and the notification goroutine.
	return stdio.Listen(ctx, in, &syncWriter{w: os.Stdout})
}

// syncWriter wraps a writer with a mutex to prevent concurrent writes
// from corrupting output. mcp-go's StdioServer writes from
// processMessage, handleNotifications and toolCallWorker; as of v0.57.0
// it serializes those itself with an internal writeMu, so this is
// belt-and-braces — it also covers anything writing to os.Stdout from
// outside the StdioServer.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (sw *syncWriter) Write(p []byte) (int, error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.w.Write(p)
}

// diagReader wraps os.Stdin so we can log when reads complete, when
// EOF arrives, and how many bytes/messages were processed before the
// connection closed. The MCP stdio transport's "the client closed
// stdin → server returns nil → process exits" path is otherwise
// indistinguishable from a clean shutdown.
type diagReader struct {
	r       io.Reader
	started time.Time
	mu      sync.Mutex
	bytes   int64
	reads   int64
	eofAt   time.Time
}

func (d *diagReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	d.mu.Lock()
	d.bytes += int64(n)
	d.reads++
	if err == io.EOF {
		d.eofAt = time.Now()
	}
	d.mu.Unlock()
	if err == io.EOF {
		fmt.Fprintf(os.Stderr,
			"caliper: stdin EOF after %s (%d reads, %d bytes total)\n",
			time.Since(d.started).Round(time.Millisecond), d.reads, d.bytes)
	} else if err != nil {
		fmt.Fprintf(os.Stderr,
			"caliper: stdin read error after %s (%d reads, %d bytes): %v\n",
			time.Since(d.started).Round(time.Millisecond), d.reads, d.bytes, err)
	}
	return n, err
}

func (d *diagReader) logExit() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.eofAt.IsZero() {
		fmt.Fprintf(os.Stderr,
			"caliper: stdin still open at exit (%d reads, %d bytes)\n",
			d.reads, d.bytes)
	}
}

// cachedHandler wraps a read-only tool handler with persistent result
// caching. The cache key is (toolName, absPath, args, source-fingerprint),
// so cache hits only happen when the same call is made against
// unchanged source. Apply only to read-only handlers — mutation tools
// (apply_transform, etc.) must always run.
func (s *Server) cachedHandler(toolName string, fn mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		args := req.GetArguments()
		path := pathFromArgs(args)
		if path == "" {
			path = s.cfg.DefaultPath()
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			return fn(ctx, req)
		}

		key, err := s.results.keyFor(toolName, absPath, args)
		if err != nil {
			return fn(ctx, req) // can't fingerprint; proceed without cache
		}

		if cached := s.results.Get(key); cached != nil {
			// Emit a single progress notification so clients that
			// watch progress tokens see the call complete instantly
			// rather than appearing to hang.
			if prog := newProgress(ctx, req); prog.active() {
				prog.send(1, 1, "served from cache")
			}
			return cached, nil
		}

		result, err := fn(ctx, req)
		// Only cache successful, non-error results. Errors may be
		// transient (config issues, races) and stale errors would be
		// confusing.
		if err == nil && result != nil && !result.IsError {
			s.results.Put(key, result)
		}
		return result, err
	}
}

// pathFromArgs extracts the analysis-target path from common arg names.
// Returns "" if no recognized path arg is set.
func pathFromArgs(args map[string]any) string {
	for _, k := range []string{"path", "file_path"} {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// safeHandler wraps a tool handler with panic recovery and logging so that
// a crash in one analysis doesn't kill the entire MCP server process.
func safeHandler(fn mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (result *mcpsdk.CallToolResult, err error) {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "caliper: panic in tool handler: %v\n", r)
				result = errorResult("internal error: %v", r)
				err = nil
			}
		}()
		return fn(ctx, req)
	}
}

func (s *Server) registerTools() {
	// addRO registers a read-only analysis tool with persistent
	// result caching wrapped around it. Repeat calls with the same
	// args against unchanged source return immediately.
	addRO := func(tool mcpsdk.Tool, handler mcpserver.ToolHandlerFunc) {
		s.mcp.AddTool(tool, safeHandler(s.cachedHandler(tool.Name, handler)))
	}
	// addMut registers a mutation tool (rewrites files). Never cached
	// — its effect depends on file state at call time, and running it
	// changes the source which naturally invalidates other entries.
	addMut := func(tool mcpsdk.Tool, handler mcpserver.ToolHandlerFunc) {
		s.mcp.AddTool(tool, safeHandler(handler))
	}

	addRO(findPatternTool, s.handleFindPattern)
	addRO(findTypeUsageTool, s.handleFindTypeUsage)
	addRO(findResourceLeakTool, s.handleFindResourceLeak)
	addRO(findUncheckedErrorTool, s.handleFindUncheckedError)
	addRO(findLockViolationTool, s.handleFindLockViolation)
	addRO(findNilDerefTool, s.handleFindNilDeref)
	addRO(checkPathPropertyTool, s.handleCheckPathProperty)
	addMut(applyTransformTool, s.handleApplyTransform)
	addMut(migrateAPITool, s.handleMigrateAPI)
	addMut(addMissingCleanupTool, s.handleAddMissingCleanup)
	addRO(explainMatchTool, s.handleExplainMatch)
	addRO(listEquivalencesTool, s.handleListEquivalences)

	// Phase 5: Pattern language.
	addRO(runPatternsTool, s.handleRunPatterns)

	// Phase 4+: Advanced analysis.
	addRO(findGoroutineLeakTool, s.handleFindGoroutineLeak)
	addRO(findSQLInjectionTool, s.handleFindSQLInjection)
	addRO(findRaceConditionTool, s.handleFindRaceCondition)
	addRO(findContextMisuseTool, s.handleFindContextMisuse)
	addRO(findTaintFlowTool, s.handleFindTaintFlow)
	addRO(findCallSitesTool, s.handleFindCallSites)
	addRO(findDeferInLoopTool, s.handleFindDeferInLoop)
	addRO(findErrorSwallowTool, s.handleFindErrorSwallow)
	addRO(findSliceAppendRaceTool, s.handleFindSliceAppendRace)
	addRO(checkInterfaceTool, s.handleCheckInterface)
	addRO(explainFindingTool, s.handleExplainFinding)

	// Cross-method lifecycle rules (issue #8).
	addRO(checkPairedCallsTool, s.handleCheckPairedCalls)
	addRO(findStaleCaptureTool, s.handleFindStaleCapture)
}
