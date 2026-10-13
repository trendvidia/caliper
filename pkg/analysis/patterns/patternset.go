// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package patterns

import (
	"fmt"
	"sort"
)

// PatternEntry is a named pattern with optional metadata.
type PatternEntry struct {
	Name        string
	Description string
	Pattern     *Pattern
	Negative    bool // if true, used for absence checks
}

// PatternSet holds a collection of named, compiled patterns.
// Patterns within a set can be registered, retrieved, and enumerated.
type PatternSet struct {
	patterns map[string]*PatternEntry
}

// NewPatternSet creates an empty pattern set.
func NewPatternSet() *PatternSet {
	return &PatternSet{patterns: make(map[string]*PatternEntry)}
}

// PatternOption configures a PatternEntry during registration.
type PatternOption func(*PatternEntry)

// POptDescription sets the entry's description.
func POptDescription(desc string) PatternOption {
	return func(e *PatternEntry) { e.Description = desc }
}

// POptTypes applies type constraints to the pattern.
func POptTypes(tc TypeConstraint) PatternOption {
	return func(e *PatternEntry) {
		e.Pattern = e.Pattern.WithTypeConstraints(tc)
	}
}

// POptNames applies name constraints to the pattern.
// Panics on invalid regex — validate before calling, or use Register
// which returns the error.
func POptNames(nc NameConstraint) PatternOption {
	return func(e *PatternEntry) {
		p, err := e.Pattern.WithNameConstraints(nc)
		if err != nil {
			// Store error in entry description as a signal; Register checks this.
			e.Description = "ERROR: " + err.Error()
			e.Pattern = nil
			return
		}
		e.Pattern = p
	}
}

// POptNegative marks the entry as a negative (absence) pattern.
func POptNegative() PatternOption {
	return func(e *PatternEntry) { e.Negative = true }
}

// Register compiles a pattern string and adds it to the set under the
// given name. Options can apply type/name constraints and other metadata.
func (ps *PatternSet) Register(name, pattern string, opts ...PatternOption) error {
	p, err := Compile(pattern)
	if err != nil {
		return fmt.Errorf("pattern %q: %w", name, err)
	}

	entry := &PatternEntry{Name: name, Pattern: p}
	for _, opt := range opts {
		opt(entry)
	}

	// Check for errors set by options (e.g., invalid regex in POptNames).
	if entry.Pattern == nil {
		return fmt.Errorf("pattern %q: %s", name, entry.Description)
	}

	ps.patterns[name] = entry
	return nil
}

// Get retrieves a named pattern entry. Returns false if not found.
func (ps *PatternSet) Get(name string) (*PatternEntry, bool) {
	e, ok := ps.patterns[name]
	return e, ok
}

// Names returns all registered pattern names in sorted order.
func (ps *PatternSet) Names() []string {
	names := make([]string, 0, len(ps.patterns))
	for name := range ps.patterns {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Len returns the number of registered patterns.
func (ps *PatternSet) Len() int {
	return len(ps.patterns)
}
