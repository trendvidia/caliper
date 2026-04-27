package gcl

import (
	"fmt"
	"os"
	"strings"

	"github.com/trendvidia/protowire/encoding/pxf"

	"github.com/trendvidia/caliper/pkg/analysis/patterns"
	pb "github.com/trendvidia/caliper/pkg/caliperpb"
)

// ResolvedRule is a rule with all pattern references resolved to compiled patterns.
type ResolvedRule struct {
	Name            string
	Description     string
	Severity        string
	Scope           string
	Message         string
	MatchPattern    *patterns.Pattern
	AbsentPattern   *patterns.Pattern // nil if not an absence rule
	ContainsPattern *patterns.Pattern // nil if not a contains rule
}

// LoadedGCL is a fully parsed and compiled .gcl file.
type LoadedGCL struct {
	Proto    *pb.GCLFile
	Patterns *patterns.PatternSet
	Rules    []ResolvedRule
	Source   string // file path
}

// Load parses a .gcl file and compiles all patterns and rules.
func Load(path string) (*LoadedGCL, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return LoadBytes(data, path)
}

// LoadBytes parses .gcl content from bytes.
func LoadBytes(data []byte, source string) (*LoadedGCL, error) {
	var file pb.GCLFile
	if err := pxf.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", source, err)
	}

	if file.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported version %d (expected 1)", source, file.Version)
	}
	if file.Name == "" {
		return nil, fmt.Errorf("%s: missing required field 'name'", source)
	}
	if len(file.Rules) == 0 {
		return nil, fmt.Errorf("%s: no rules defined", source)
	}

	// Build PatternSet from named patterns.
	ps := patterns.NewPatternSet()
	for name, gp := range file.Patterns {
		var opts []patterns.PatternOption
		if gp.Description != "" {
			opts = append(opts, patterns.POptDescription(gp.Description))
		}
		if len(gp.TypeConstraints) > 0 {
			opts = append(opts, patterns.POptTypes(patterns.TypeConstraint(gp.TypeConstraints)))
		}
		if len(gp.NameConstraints) > 0 {
			opts = append(opts, patterns.POptNames(patterns.NameConstraint(gp.NameConstraints)))
		}
		if err := ps.Register(name, gp.Pattern, opts...); err != nil {
			return nil, fmt.Errorf("%s: pattern %q: %w", source, name, err)
		}
	}

	// Resolve rules.
	rules := make([]ResolvedRule, 0, len(file.Rules))
	for _, r := range file.Rules {
		resolved, err := resolveRule(r, ps)
		if err != nil {
			return nil, fmt.Errorf("%s: rule %q: %w", source, r.Name, err)
		}
		rules = append(rules, resolved)
	}

	return &LoadedGCL{
		Proto:    &file,
		Patterns: ps,
		Rules:    rules,
		Source:   source,
	}, nil
}

func resolveRule(r *pb.Rule, ps *patterns.PatternSet) (ResolvedRule, error) {
	resolved := ResolvedRule{
		Name:        r.Name,
		Description: r.Description,
		Severity:    r.Severity,
		Scope:       r.Scope,
		Message:     r.Message,
	}
	if resolved.Scope == "" {
		resolved.Scope = "function"
	}
	if resolved.Severity == "" {
		resolved.Severity = "warning"
	}

	var err error
	resolved.MatchPattern, err = resolvePatternRef(r.Match, ps)
	if err != nil {
		return resolved, fmt.Errorf("match: %w", err)
	}

	if r.Absent != "" {
		resolved.AbsentPattern, err = resolvePatternRef(r.Absent, ps)
		if err != nil {
			return resolved, fmt.Errorf("absent: %w", err)
		}
	}

	if r.Contains != "" {
		resolved.ContainsPattern, err = resolvePatternRef(r.Contains, ps)
		if err != nil {
			return resolved, fmt.Errorf("contains: %w", err)
		}
	}

	return resolved, nil
}

func resolvePatternRef(ref string, ps *patterns.PatternSet) (*patterns.Pattern, error) {
	if strings.HasPrefix(ref, "@") {
		name := ref[1:]
		entry, ok := ps.Get(name)
		if !ok {
			return nil, fmt.Errorf("unknown pattern reference @%s", name)
		}
		return entry.Pattern, nil
	}
	// Inline pattern.
	return patterns.Compile(ref)
}
