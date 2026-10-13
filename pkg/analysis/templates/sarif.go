// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

package templates

import (
	"encoding/json"
	"fmt"
)

// RenderSARIF emits a SARIF v2.1.0 report for the given findings as
// JSON-encoded bytes. Suitable for upload to GitHub Code Scanning, for
// piping into tooling that consumes SARIF (Steward's ADR-007 gate,
// reviewdog, etc.), or for printing on stdout.
//
// `version` is stamped into runs[].tool.driver.version. Pass the
// build-time Caliper version when known; "dev" or "" are accepted but
// produce a less-useful provenance trail.
//
// Severity mapping mirrors the SARIF level enum:
//
//	Caliper "error"   → SARIF "error"
//	Caliper "warning" → SARIF "warning"
//	Caliper "info"    → SARIF "note"
//	(anything else)   → SARIF "warning" (safe fallback)
//
// Caliper's `Suggestion` field surfaces as a SARIF `fixes[]` entry;
// `Explanation` and `Path` (CFG block IDs) are not emitted today —
// SARIF has no canonical home for them and most consumers ignore
// non-standard fields.
func RenderSARIF(findings []Finding, version string) ([]byte, error) {
	if version == "" {
		version = "dev"
	}

	// Collect unique rule IDs in first-seen order so the output is
	// deterministic for a given findings slice.
	ruleMap := map[string]int{}
	rules := make([]map[string]any, 0)
	for _, f := range findings {
		if _, ok := ruleMap[f.Kind]; ok {
			continue
		}
		ruleMap[f.Kind] = len(rules)
		rules = append(rules, map[string]any{
			"id": f.Kind,
			"shortDescription": map[string]any{
				"text": f.Kind,
			},
		})
	}

	sevMap := map[string]string{
		"error":   "error",
		"warning": "warning",
		"info":    "note",
	}

	results := make([]map[string]any, 0, len(findings))
	for _, f := range findings {
		level := sevMap[f.Severity]
		if level == "" {
			level = "warning"
		}

		result := map[string]any{
			"ruleId":  f.Kind,
			"level":   level,
			"message": map[string]any{"text": f.Message},
			"locations": []map[string]any{{
				"physicalLocation": map[string]any{
					"artifactLocation": map[string]any{
						"uri": f.Location.File,
					},
					"region": map[string]any{
						"startLine":   f.Location.Line,
						"startColumn": f.Location.Column,
					},
				},
			}},
		}
		if f.Suggestion != "" {
			result["fixes"] = []map[string]any{{
				"description": map[string]any{"text": f.Suggestion},
			}}
		}
		results = append(results, result)
	}

	sarif := map[string]any{
		"version": "2.1.0",
		"$schema": "https://raw.githubusercontent.com/oasis-tcs/sarif-spec/main/sarif-2.1/schema/sarif-schema-2.1.0.json",
		"runs": []map[string]any{{
			"tool": map[string]any{
				"driver": map[string]any{
					"name":    "Caliper",
					"version": version,
					"rules":   rules,
				},
			},
			"results": results,
		}},
	}

	out, err := json.MarshalIndent(sarif, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode sarif: %w", err)
	}
	return out, nil
}
