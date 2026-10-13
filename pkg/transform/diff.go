// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package transform

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// FileDiff is a unified diff for a single file.
type FileDiff struct {
	File string `json:"file"`
	Diff string `json:"diff"`
}

// UnifiedDiff generates unified diffs grouped by file from a Result.
// Each FileDiff contains a standard unified diff that LLMs and tools
// like `patch` can consume. contextLines controls how many unchanged
// lines surround each change (default 3).
func (r *Result) UnifiedDiff(contextLines int) []FileDiff {
	if contextLines < 0 {
		contextLines = 3
	}

	byFile := make(map[string][]Change)
	for _, c := range r.Changes {
		byFile[c.File] = append(byFile[c.File], c)
	}

	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)

	var diffs []FileDiff
	for _, file := range files {
		changes := byFile[file]
		diff := generateFileDiff(file, changes, contextLines)
		if diff != "" {
			diffs = append(diffs, FileDiff{File: file, Diff: diff})
		}
	}
	return diffs
}

// generateFileDiff produces a unified diff for one file.
func generateFileDiff(file string, changes []Change, ctx int) string {
	src, err := os.ReadFile(file)
	if err != nil {
		return generateSyntheticDiff(file, changes)
	}

	origLines := strings.Split(string(src), "\n")

	sort.Slice(changes, func(i, j int) bool {
		return changes[i].Line < changes[j].Line
	})

	// Build modified file by applying replacements.
	modLines := applyChanges(origLines, changes)

	return computeUnifiedDiff(file, origLines, modLines, ctx)
}

// applyChanges applies a sorted set of Changes to original lines,
// returning the modified lines.
func applyChanges(origLines []string, changes []Change) []string {
	var mod []string
	origIdx := 0 // 0-indexed

	for _, c := range changes {
		startIdx := c.Line - 1 // convert to 0-indexed
		if startIdx < 0 {
			startIdx = 0
		}

		// Copy unchanged lines before this change.
		for origIdx < startIdx && origIdx < len(origLines) {
			mod = append(mod, origLines[origIdx])
			origIdx++
		}

		oldLines := splitLines(c.Before)
		newLines := splitLines(c.After)

		// Skip the original lines that match "before".
		skip := len(oldLines)
		if skip < 1 {
			skip = 1
		}

		// Add replacement lines.
		mod = append(mod, newLines...)
		origIdx += skip
	}

	// Copy remaining lines.
	for origIdx < len(origLines) {
		mod = append(mod, origLines[origIdx])
		origIdx++
	}

	return mod
}

// computeUnifiedDiff computes a unified diff between two line slices.
func computeUnifiedDiff(file string, a, b []string, ctx int) string {
	ops := lcsEditOps(a, b)

	// Check if there are any actual changes.
	hasChange := false
	for _, op := range ops {
		if op != '=' {
			hasChange = true
			break
		}
	}
	if !hasChange {
		return ""
	}

	var buf strings.Builder
	fmt.Fprintf(&buf, "--- a/%s\n", file)
	fmt.Fprintf(&buf, "+++ b/%s\n", file)

	var entries []opEntry
	ai, bi := 0, 0
	for _, op := range ops {
		switch op {
		case '=':
			entries = append(entries, opEntry{'=', a[ai], ai, bi})
			ai++
			bi++
		case '-':
			entries = append(entries, opEntry{'-', a[ai], ai, -1})
			ai++
		case '+':
			entries = append(entries, opEntry{'+', b[bi], -1, bi})
			bi++
		}
	}

	// Find changed regions and expand with context to form hunks.
	hunks := buildHunks(entries, ctx)
	for _, h := range hunks {
		fmt.Fprintf(&buf, "@@ -%d,%d +%d,%d @@\n", h.aStart+1, h.aCount, h.bStart+1, h.bCount)
		for _, l := range h.lines {
			buf.WriteString(l)
			buf.WriteByte('\n')
		}
	}

	return buf.String()
}

type hunk struct {
	aStart int
	aCount int
	bStart int
	bCount int
	lines  []string
}

type opEntry struct {
	kind byte
	line string
	aIdx int
	bIdx int
}

func buildHunks(entries []opEntry, ctx int) []hunk {
	// Find indices of changed entries.
	var changedIdxs []int
	for i, e := range entries {
		if e.kind != '=' {
			changedIdxs = append(changedIdxs, i)
		}
	}
	if len(changedIdxs) == 0 {
		return nil
	}

	// Group changed indices into ranges separated by more than 2*ctx equal lines.
	type idxRange struct{ start, end int }
	var groups []idxRange
	groupStart := changedIdxs[0]
	groupEnd := changedIdxs[0]

	for i := 1; i < len(changedIdxs); i++ {
		if changedIdxs[i]-groupEnd > 2*ctx+1 {
			groups = append(groups, idxRange{groupStart, groupEnd})
			groupStart = changedIdxs[i]
		}
		groupEnd = changedIdxs[i]
	}
	groups = append(groups, idxRange{groupStart, groupEnd})

	// Build a hunk for each group.
	var hunks []hunk
	for _, g := range groups {
		start := g.start - ctx
		if start < 0 {
			start = 0
		}
		end := g.end + ctx
		if end >= len(entries) {
			end = len(entries) - 1
		}

		var h hunk
		h.aStart = -1
		h.bStart = -1

		for i := start; i <= end; i++ {
			e := entries[i]

			switch e.kind {
			case '=':
				if h.aStart == -1 {
					h.aStart = e.aIdx
					h.bStart = e.bIdx
				}
				h.lines = append(h.lines, " "+e.line)
				h.aCount++
				h.bCount++
			case '-':
				if h.aStart == -1 {
					h.aStart = e.aIdx
					h.bStart = firstBIdx(entries, i)
				}
				h.lines = append(h.lines, "-"+e.line)
				h.aCount++
			case '+':
				if h.aStart == -1 {
					h.aStart = firstAIdx(entries, i)
					h.bStart = e.bIdx
				}
				h.lines = append(h.lines, "+"+e.line)
				h.bCount++
			}
		}

		if h.aStart == -1 {
			h.aStart = 0
		}
		if h.bStart == -1 {
			h.bStart = 0
		}

		hunks = append(hunks, h)
	}
	return hunks
}

func firstBIdx(entries []opEntry, from int) int {
	for i := from; i < len(entries); i++ {
		if entries[i].bIdx >= 0 {
			return entries[i].bIdx
		}
	}
	// Search backward.
	for i := from - 1; i >= 0; i-- {
		if entries[i].bIdx >= 0 {
			return entries[i].bIdx + 1
		}
	}
	return 0
}

func firstAIdx(entries []opEntry, from int) int {
	for i := from; i < len(entries); i++ {
		if entries[i].aIdx >= 0 {
			return entries[i].aIdx
		}
	}
	for i := from - 1; i >= 0; i-- {
		if entries[i].aIdx >= 0 {
			return entries[i].aIdx + 1
		}
	}
	return 0
}

// lcsEditOps returns the edit operations ('=', '-', '+') to transform a into b.
func lcsEditOps(a, b []string) []byte {
	m, n := len(a), len(b)

	// Build LCS length table.
	tbl := make([][]int, m+1)
	for i := range tbl {
		tbl[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if a[i] == b[j] {
				tbl[i][j] = tbl[i+1][j+1] + 1
			} else if tbl[i+1][j] >= tbl[i][j+1] {
				tbl[i][j] = tbl[i+1][j]
			} else {
				tbl[i][j] = tbl[i][j+1]
			}
		}
	}

	var ops []byte
	i, j := 0, 0
	for i < m && j < n {
		if a[i] == b[j] {
			ops = append(ops, '=')
			i++
			j++
		} else if tbl[i+1][j] >= tbl[i][j+1] {
			ops = append(ops, '-')
			i++
		} else {
			ops = append(ops, '+')
			j++
		}
	}
	for i < m {
		ops = append(ops, '-')
		i++
	}
	for j < n {
		ops = append(ops, '+')
		j++
	}
	return ops
}

// splitLines splits text into lines, returning nil for empty input.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// generateSyntheticDiff creates a diff from Changes alone when the
// original file can't be read.
func generateSyntheticDiff(file string, changes []Change) string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "--- a/%s\n", file)
	fmt.Fprintf(&buf, "+++ b/%s\n", file)

	for _, c := range changes {
		oldLines := splitLines(c.Before)
		newLines := splitLines(c.After)
		oldCount := len(oldLines)
		newCount := len(newLines)
		if oldCount == 0 {
			oldCount = 1
		}
		if newCount == 0 {
			newCount = 1
		}

		fmt.Fprintf(&buf, "@@ -%d,%d +%d,%d @@\n", c.Line, oldCount, c.Line, newCount)
		for _, l := range oldLines {
			fmt.Fprintf(&buf, "-%s\n", l)
		}
		for _, l := range newLines {
			fmt.Fprintf(&buf, "+%s\n", l)
		}
	}
	return buf.String()
}
