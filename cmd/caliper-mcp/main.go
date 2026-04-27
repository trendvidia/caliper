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

// Version is the release identifier reported in the MCP initialize
// handshake. Set at build time via:
//
//	go build -ldflags "-X main.Version=v1.2.3" ./cmd/caliper-mcp
//
// When unset, resolveVersion() falls back to the VCS revision and
// dirty-tree status embedded by the Go toolchain.
var Version = ""

// resolveVersion returns the Version override if set, otherwise derives
// a version string from runtime/debug.BuildInfo (e.g., "dev-1a2b3c4"
// or "dev-1a2b3c4-dirty"). Falls back to "dev" if no VCS metadata is
// embedded (go run, go install with no VCS, etc.).
func resolveVersion() string {
	if Version != "" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 7 {
				rev = s.Value[:7]
			} else {
				rev = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev == "" {
		return "dev"
	}
	return "dev-" + rev + dirty
}

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

	version := resolveVersion()

	srv, err := mcp.NewServer(version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "caliper: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "caliper: server starting %s (pid %d)\n", version, os.Getpid())

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
		fmt.Fprintf(os.Stderr, "caliper: stderr pipe failed: %v\n", err)
		_ = f.Close() //caliper:ignore unchecked_error error_swallow
		return func() {}
	}

	os.Stderr = wr
	// Prefix log lines with timestamps so disconnects can be correlated
	// with the user's actions in the MCP transcript. The original stderr
	// (the MCP client) gets the raw bytes — only the file gets the
	// prefix to keep stderr-as-protocol-channel uncluttered.
	mw := io.MultiWriter(origStderr, &lineStampWriter{w: f})

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(mw, rd) //caliper:ignore error_swallow
		close(done)
	}()

	fmt.Fprintf(os.Stderr, "caliper: stderr teed to %s\n", logPath)

	return func() {
		_ = wr.Close() //caliper:ignore error_swallow
		<-done
		_ = f.Close() //caliper:ignore error_swallow
		os.Stderr = origStderr
	}
}

// lineStampWriter prefixes every newline-terminated line with an
// RFC3339-with-millis timestamp. Partial writes (no trailing newline)
// are buffered until the line completes so the prefix lands at the
// start of each line, not mid-line.
type lineStampWriter struct {
	w   io.Writer
	buf []byte
}

func (l *lineStampWriter) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		nl := -1
		for i, b := range l.buf {
			if b == '\n' {
				nl = i
				break
			}
		}
		if nl < 0 {
			break
		}
		stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z ")
		out := append([]byte(stamp), l.buf[:nl+1]...)
		if _, err := l.w.Write(out); err != nil {
			return len(p), err
		}
		l.buf = l.buf[nl+1:]
	}
	return len(p), nil
}
