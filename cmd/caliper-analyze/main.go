// Command caliper-analyze is the single-shot CLI counterpart to caliper-mcp.
//
// Where caliper-mcp is a long-lived stdio JSON-RPC server intended for
// LLM-driven interactive use, caliper-analyze runs a curated set of
// analyzers once against a Go module and prints SARIF v2.1.0 to stdout.
// It exists for non-LLM consumers — CI gates (Steward's ADR-007 SARIF
// adapter), reviewdog, GitHub Code Scanning uploads — where MCP's
// session model adds protocol overhead with no benefit.
//
// Usage:
//
//	caliper-analyze \
//	  --workspace ./path/to/module \
//	  --rules nil_deref,race_condition,sql_injection \
//	  --format sarif \
//	  > findings.sarif
//
// `--workspace` defaults to the current working directory.
// `--rules` defaults to templates.DefaultKinds (the curated PR rule set).
// `--format` defaults to "sarif"; "json" emits the raw []templates.Finding.
//
// The workspace must be a buildable Go module (go.mod plus sources). The
// project's `.caliper.pxf` (walked up from --workspace) is honored —
// severity overrides, rule enable/disable, and infallible-method
// allowlists apply just like under MCP.
//
// Exit codes:
//
//	0 — analysis ran to completion. Findings are reported in stdout;
//	    presence of findings does NOT influence exit. Caller decides
//	    what's blocking based on severity / count.
//	1 — fatal error (couldn't load packages, bad flags, write error).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/trendvidia/caliper/pkg/analysis"
	"github.com/trendvidia/caliper/pkg/analysis/templates"
	"github.com/trendvidia/caliper/pkg/config"
)

// Version is the release identifier stamped into SARIF output's
// runs[].tool.driver.version. Set at build time via:
//
//	go build -ldflags "-X main.Version=v1.2.3" ./cmd/caliper-analyze
//
// When unset, resolveVersion() falls back to VCS-revision metadata
// (matching cmd/caliper-mcp's convention).
var Version = ""

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
	if rev != "" {
		return "dev-" + rev + dirty
	}
	// No VCS metadata means this was not built from a working tree.
	// `go install pkg@version` is the case that matters: it builds from
	// the module cache and stamps the exact released version here.
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return "dev"
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "caliper-analyze: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	workspace := flag.String("workspace", ".", "Path to the Go module to analyze (must contain go.mod)")
	rulesFlag := flag.String("rules", "", "Comma-separated rule kinds to run (default: all PR-default rules)")
	format := flag.String("format", "sarif", `Output format: "sarif" or "json"`)
	versionFlag := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Println(resolveVersion())
		return nil
	}

	rules, err := resolveRules(*rulesFlag)
	if err != nil {
		return err
	}

	// Project-local .caliper.pxf is walked up from the workspace path —
	// same resolution the MCP server uses (pkg/mcp/engine.go:88).
	cfg, err := config.Load(*workspace)
	if err != nil {
		return fmt.Errorf("load .caliper.pxf: %w", err)
	}
	state := templates.NewInfallibleState(cfg.Infallible())

	engine, err := analysis.NewEngine(*workspace, "./...")
	if err != nil {
		return fmt.Errorf("load packages from %q: %w", *workspace, err)
	}

	var findings []templates.Finding
	for _, kind := range rules {
		tmpl := templates.ByKind(kind)
		if tmpl == nil {
			// Already validated in resolveRules but defense-in-depth: skip
			// rather than crash if ByKind grows new "needs params" cases.
			fmt.Fprintf(os.Stderr, "caliper-analyze: skipping rule %q (no zero-arg constructor)\n", kind)
			continue
		}
		findings = append(findings, templates.RunTemplateWithConfig(engine, tmpl, state)...)
	}

	switch *format {
	case "sarif":
		out, err := templates.RenderSARIF(findings, resolveVersion())
		if err != nil {
			return fmt.Errorf("render sarif: %w", err)
		}
		_, err = os.Stdout.Write(append(out, '\n'))
		return err
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(findings)
	default:
		return fmt.Errorf("unknown --format %q (want sarif or json)", *format)
	}
}

// resolveRules parses --rules into a validated kind list. Empty input
// yields templates.DefaultKinds (the PR-default set). Unknown kinds —
// or kinds that ByKind cannot construct without params (resource_leak)
// — produce a flag error so the operator notices typos at startup
// instead of silently getting an empty findings list.
func resolveRules(input string) ([]string, error) {
	if strings.TrimSpace(input) == "" {
		return templates.DefaultKinds, nil
	}
	var out []string
	for _, raw := range strings.Split(input, ",") {
		k := strings.TrimSpace(raw)
		if k == "" {
			continue
		}
		if templates.ByKind(k) == nil {
			return nil, fmt.Errorf("unknown or unconstructible rule kind %q", k)
		}
		out = append(out, k)
	}
	if len(out) == 0 {
		return templates.DefaultKinds, nil
	}
	return out, nil
}
