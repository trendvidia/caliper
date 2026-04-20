// Package config handles loading and applying .gocaliper.yaml configuration.
//
// The config file lets users customize severity levels, enable/disable
// specific analysis rules, set default paths, and configure engine options
// without passing them on every MCP tool call.
package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// GCLPatterns returns the configured .gcl pattern file paths, resolved
// relative to the given base directory. Supports glob patterns.
func (c *Config) GCLPatterns(baseDir string) []string {
	if c == nil || len(c.Patterns) == 0 {
		return nil
	}
	var results []string
	for _, pattern := range c.Patterns {
		// Resolve relative to base directory.
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(baseDir, pattern)
		}
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		results = append(results, matches...)
	}
	return results
}

// Config is the top-level configuration from .gocaliper.yaml.
type Config struct {
	// Severity overrides per rule kind (e.g., "resource_leak": "error").
	Severity map[string]string `yaml:"severity,omitempty"`

	// Rules controls which analysis rules are enabled or disabled.
	Rules RulesConfig `yaml:"rules,omitempty"`

	// Engine configures package loading behavior.
	Engine EngineConfig `yaml:"engine,omitempty"`

	// Defaults sets default parameter values for tool calls.
	Defaults DefaultsConfig `yaml:"defaults,omitempty"`

	// Patterns lists paths to .gcl pattern files. Supports globs.
	// Relative paths are resolved from the config file's directory.
	Patterns []string `yaml:"patterns,omitempty"`
}

// RulesConfig controls which rules are active.
type RulesConfig struct {
	// Enable lists rules to run. If non-empty, only these rules are active.
	Enable []string `yaml:"enable,omitempty"`
	// Disable lists rules to skip. Applied after Enable.
	Disable []string `yaml:"disable,omitempty"`
}

// EngineConfig controls package loading.
type EngineConfig struct {
	// SkipGenerated skips files with "// Code generated" headers.
	// Default: true.
	SkipGenerated *bool `yaml:"skip_generated,omitempty"`
	// SkipCgo skips files that import "C". Default: true.
	SkipCgo *bool `yaml:"skip_cgo,omitempty"`
	// BuildTags sets build tags for package loading.
	BuildTags []string `yaml:"build_tags,omitempty"`
	// MaxCacheEntries controls the LRU engine cache size. Default: 8.
	MaxCacheEntries int `yaml:"max_cache_entries,omitempty"`
}

// DefaultsConfig provides default values for tool parameters.
type DefaultsConfig struct {
	// Path is the default analysis path if none is specified.
	Path string `yaml:"path,omitempty"`
	// Preview controls whether transforms default to preview mode.
	// Default: true.
	Preview *bool `yaml:"preview,omitempty"`
}

// Load reads .gocaliper.yaml from the given directory or any parent
// directory, returning a zero Config if no file is found.
func Load(dir string) (*Config, error) {
	path := findConfigFile(dir)
	if path == "" {
		return &Config{}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// findConfigFile walks up from dir looking for .gocaliper.yaml.
func findConfigFile(dir string) string {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}

	for {
		path := filepath.Join(dir, ".gocaliper.yaml")
		if _, err := os.Stat(path); err == nil {
			return path
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// SeverityFor returns the configured severity for a rule kind,
// falling back to the provided default.
func (c *Config) SeverityFor(kind, defaultSeverity string) string {
	if c == nil || c.Severity == nil {
		return defaultSeverity
	}
	if s, ok := c.Severity[kind]; ok {
		return s
	}
	return defaultSeverity
}

// RuleEnabled reports whether a rule is active given the enable/disable lists.
func (c *Config) RuleEnabled(name string) bool {
	if c == nil {
		return true
	}

	// If enable list is set, rule must be in it.
	if len(c.Rules.Enable) > 0 {
		found := false
		for _, r := range c.Rules.Enable {
			if r == name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	// Check disable list.
	for _, r := range c.Rules.Disable {
		if r == name {
			return false
		}
	}
	return true
}

// DefaultPath returns the configured default path, or "." if not set.
func (c *Config) DefaultPath() string {
	if c != nil && c.Defaults.Path != "" {
		return c.Defaults.Path
	}
	return "."
}

// SkipGenerated returns the configured value, defaulting to true.
func (c *Config) SkipGenerated() bool {
	if c != nil && c.Engine.SkipGenerated != nil {
		return *c.Engine.SkipGenerated
	}
	return true
}

// SkipCgo returns the configured value, defaulting to true.
func (c *Config) SkipCgo() bool {
	if c != nil && c.Engine.SkipCgo != nil {
		return *c.Engine.SkipCgo
	}
	return true
}
