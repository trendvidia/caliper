// Package config handles loading and applying .gocaliper.pxf configuration.
//
// The config file lets users customize severity levels, enable/disable
// specific analysis rules, set default paths, and configure engine options
// without passing them on every MCP tool call. Configuration is stored in
// PXF format (Proto eXpressive Format) backed by protobuf schemas.
package config

import (
	"os"
	"path/filepath"

	"github.com/trendvidia/protowire/encoding/pxf"

	pb "github.com/trendvidia/gocaliper/pkg/gocaliperpb"
)

// Config wraps the protobuf config message with convenience methods.
type Config struct {
	pb *pb.Config
}

// Load reads .gocaliper.pxf from the given directory or any parent
// directory, returning a zero Config if no file is found.
func Load(dir string) (*Config, error) {
	path := findConfigFile(dir)
	if path == "" {
		return &Config{pb: &pb.Config{}}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var msg pb.Config
	if err := pxf.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &Config{pb: &msg}, nil
}

// findConfigFile walks up from dir looking for .gocaliper.pxf.
func findConfigFile(dir string) string {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}

	for {
		path := filepath.Join(dir, ".gocaliper.pxf")
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
	if c == nil || c.pb == nil || len(c.pb.Severity) == 0 {
		return defaultSeverity
	}
	if s, ok := c.pb.Severity[kind]; ok {
		return s
	}
	return defaultSeverity
}

// RuleEnabled reports whether a rule is active given the enable/disable lists.
func (c *Config) RuleEnabled(name string) bool {
	if c == nil || c.pb == nil {
		return true
	}

	rules := c.pb.Rules
	if rules == nil {
		return true
	}

	// If enable list is set, rule must be in it.
	if len(rules.Enable) > 0 {
		found := false
		for _, r := range rules.Enable {
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
	for _, r := range rules.Disable {
		if r == name {
			return false
		}
	}
	return true
}

// DefaultPath returns the configured default path, or "." if not set.
func (c *Config) DefaultPath() string {
	if c != nil && c.pb != nil && c.pb.Defaults != nil && c.pb.Defaults.Path != "" {
		return c.pb.Defaults.Path
	}
	return "."
}

// SkipGenerated returns the configured value, defaulting to true.
func (c *Config) SkipGenerated() bool {
	if c != nil && c.pb != nil && c.pb.Engine != nil {
		return c.pb.Engine.SkipGenerated
	}
	return true
}

// SkipCgo returns the configured value, defaulting to true.
func (c *Config) SkipCgo() bool {
	if c != nil && c.pb != nil && c.pb.Engine != nil {
		return c.pb.Engine.SkipCgo
	}
	return true
}

// BuildTags returns the configured build tags.
func (c *Config) BuildTags() []string {
	if c != nil && c.pb != nil && c.pb.Engine != nil {
		return c.pb.Engine.BuildTags
	}
	return nil
}

// MaxCacheEntries returns the configured cache size.
func (c *Config) MaxCacheEntries() int {
	if c != nil && c.pb != nil && c.pb.Engine != nil {
		return int(c.pb.Engine.MaxCacheEntries)
	}
	return 0
}

// GCLPatterns returns the configured .gcl pattern file paths, resolved
// relative to the given base directory. Supports glob patterns.
func (c *Config) GCLPatterns(baseDir string) []string {
	if c == nil || c.pb == nil || len(c.pb.Patterns) == 0 {
		return nil
	}
	var results []string
	for _, pattern := range c.pb.Patterns {
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
