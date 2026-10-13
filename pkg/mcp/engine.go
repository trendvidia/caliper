// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package mcp

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/trendvidia/caliper/pkg/analysis"
	"github.com/trendvidia/caliper/pkg/analysis/templates"
	"github.com/trendvidia/caliper/pkg/config"
)

// Engine plumbing: defaultPath, getEngine, getEngineWithProgress,
// infallibleStateFor, runTemplate, invalidateEngine.

// defaultPath returns the given path or the configured default.
func (s *Server) defaultPath(path string) string {
	if path != "" {
		return path
	}
	return s.cfg.DefaultPath()
}

// getEngine returns a cached engine for the given path, creating one on cache miss.
//
// The returned release function MUST be called (typically via defer) once
// the caller is done using the engine. The returned mutex is held across
// the engine's lifetime in this call, serializing concurrent tool calls
// on the same codebase. This is required because go/types maps embedded
// in the engine are not safe for concurrent reads.
//
// On error, release is a no-op closure (still safe to defer).
func (s *Server) getEngine(path string) (*analysis.Engine, func(), error) {
	return s.getEngineWithProgress(path, nil)
}

// getEngineWithProgress is like getEngine but sends progress notifications
// during package loading when a progress token is present.
func (s *Server) getEngineWithProgress(path string, prog *progress) (*analysis.Engine, func(), error) {
	noop := func() {}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, noop, fmt.Errorf("resolving path: %w", err)
	}

	unlock := s.cache.lock(absPath)
	// From here on, on any error path, unlock() must be called before
	// returning. On the success path, ownership of unlock transfers to
	// the caller, who is responsible for calling it once analysis is done.

	if eng := s.cache.get(absPath); eng != nil {
		return eng, unlock, nil
	}

	if prog != nil {
		prog.send(0, 3, "Loading packages...")
	}

	opts := analysis.EngineOptions{
		SkipGenerated: s.cfg.SkipGenerated(),
		SkipCgo:       s.cfg.SkipCgo(),
		BuildTags:     s.cfg.BuildTags(),
	}
	eng, err := analysis.NewEngineWithOptions(absPath, opts)
	if err != nil {
		unlock()
		return nil, noop, err
	}

	if prog != nil {
		prog.send(1, 3, "Packages loaded")
	}

	// Resolve project-local config by walking up from the analysis path.
	// Falls back to the server-boot config if no project file is found —
	// the boot config still governs cache size, severity, etc., and
	// supplies an Infallible block as a global default.
	state := s.infallibleStateFor(absPath)
	s.cache.put(absPath, eng, state)
	return eng, unlock, nil
}

// infallibleStateFor returns the project-local InfallibleState for the
// given analysis path: walks up from absPath looking for .caliper.pxf,
// falling back to the server-boot config if no project-local file
// exists. The two are NOT merged — a project file fully overrides.
func (s *Server) infallibleStateFor(absPath string) *templates.InfallibleState {
	cfg, err := config.Load(absPath)
	if err != nil {
		// Fall back to boot config silently — config-load errors
		// shouldn't crash analysis.
		fmt.Fprintf(os.Stderr, "caliper: load project config from %s: %v\n", absPath, err)
		return templates.NewInfallibleState(s.cfg.Infallible())
	}
	if inf := cfg.Infallible(); inf != nil {
		return templates.NewInfallibleState(inf)
	}
	return templates.NewInfallibleState(s.cfg.Infallible())
}

// runTemplate runs tmpl against engine with the project-local
// InfallibleState resolved for path. Wraps templates.RunTemplateWithConfig
// so handlers don't need to thread the state through themselves.
func (s *Server) runTemplate(path string, engine *analysis.Engine, tmpl templates.Template) []templates.Finding {
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}
	state := s.cache.infallibleFor(absPath)
	return templates.RunTemplateWithConfig(engine, tmpl, state)
}

// invalidateEngine removes the cached engine for the given path.
// Must be called after transforms that modify files on disk.
func (s *Server) invalidateEngine(path string) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}
	s.cache.invalidate(absPath)
}
