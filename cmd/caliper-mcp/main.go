// Command caliper-mcp runs the Caliper MCP server.
//
// Caliper is a semantic code analysis and transformation tool for Go,
// inspired by Coccinelle. It exposes high-level, intent-based tools over MCP
// so that an LLM can request precise, AST-level and control-flow-level
// operations without needing to understand the formal logic underneath.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/trendvidia/caliper/pkg/mcp"
)

func main() {
	// Tee stderr to a logfile so disconnect causes (panics, signals,
	// transport errors) survive across MCP reconnects. The logfile is
	// shared across runs (append) and includes timestamps + PID so
	// multiple sessions don't overwrite each other.
	closeLog := setupStderrTee()
	defer closeLog()

	// Ensure full stack traces on fatal crashes (concurrent map access, etc.)
	// so the root cause can be diagnosed from MCP client logs.
	if err := os.Setenv("GOTRACEBACK", "crash"); err != nil {
		fmt.Fprintf(os.Stderr, "caliper: setting GOTRACEBACK: %v\n", err)
	}

	// Top-level panic recovery — last resort before process death.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "caliper: fatal panic: %v\n%s\n", r, debug.Stack())
			os.Exit(2)
		}
	}()

	// Catch SIGTERM/SIGHUP/SIGPIPE in addition to SIGINT so we can log
	// the cause when something kills the process (OS, parent, broken
	// stdout pipe).
	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
	defer cancel()

	srv, err := mcp.NewServer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "caliper: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "caliper: server starting (pid %d)\n", os.Getpid())

	if err := srv.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "caliper: server.Run returned: %v\n", err)
		os.Exit(1)
	}

	if cause := context.Cause(ctx); cause != nil {
		fmt.Fprintf(os.Stderr, "caliper: server stopped: %v\n", cause)
	} else {
		fmt.Fprintf(os.Stderr, "caliper: server stopped (clean exit)\n")
	}
}

// setupStderrTee duplicates everything written to os.Stderr into a
// per-day logfile under $TMPDIR. Returns a closer the caller defers.
//
// Why: when caliper runs as an MCP subprocess, its stderr is captured
// by the client (Claude Code, etc.) but not surfaced to the user.
// Disconnects therefore appear silent. Persisting stderr to a known
// path lets a human inspect the cause across MCP reconnects.
func setupStderrTee() func() {
	dir := os.TempDir()
	logPath := filepath.Join(dir, fmt.Sprintf("caliper-mcp-%s.log",
		time.Now().Format("2006-01-02")))

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		// Fall back silently — tee is diagnostic, not load-bearing.
		fmt.Fprintf(os.Stderr, "caliper: stderr tee disabled: %v\n", err)
		return func() {}
	}

	origStderr := os.Stderr

	// Pipe os.Stderr through a goroutine that fans out to (a) the
	// original stderr (the MCP client), and (b) the log file.
	rd, wr, err := os.Pipe()
	if err != nil {
		fmt.Fprintf(origStderr, "caliper: stderr pipe failed: %v\n", err)
		f.Close()
		return func() {}
	}

	os.Stderr = wr
	mw := io.MultiWriter(origStderr, f)

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(mw, rd)
		close(done)
	}()

	fmt.Fprintf(os.Stderr, "caliper: stderr teed to %s\n", logPath)

	return func() {
		_ = wr.Close()
		<-done
		_ = f.Close()
		os.Stderr = origStderr
	}
}
