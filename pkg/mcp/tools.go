package mcp

// This file defines every MCP tool that GoCaliper exposes.
//
// Design principle: the LLM never writes formal logic (CTL, path quantifiers).
// Instead, each tool encapsulates the formal reasoning and exposes an
// intent-based interface with natural parameters. The tool descriptions are
// deliberately verbose — they ARE the training data for the LLM.

import (
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// --------------------------------------------------------------------------
// 1. Pattern Matching — the 80%
// --------------------------------------------------------------------------

var findPatternTool = mcpsdk.Tool{
	Name: "find_pattern",
	Description: `Find Go code matching a structural AST pattern. This is NOT a text search —
it understands Go syntax and matches based on code structure.

PATTERN SYNTAX:
  $x       — metavariable: matches any expression/identifier and captures it as "x"
  $_       — wildcard: matches any expression but does not capture
  $x...    — variadic: matches zero or more arguments/elements
  ...      — statement wildcard: matches zero or more statements in a block

EXAMPLES:
  "$x.Close()"                          → any Close() call on any receiver
  "if $err != nil { ... }"              → any error check block
  "fmt.Errorf($msg, $args...)"          → any fmt.Errorf call with any arguments
  "func($params...) ($results...) { ... }" → any function literal
  "$x, $err := $fn($args...)"           → any two-return-value assignment

TYPE CONSTRAINTS (optional):
  You can restrict metavariables to specific types. For example,
  setting type_constraints to {"x": "*os.File"} with pattern "$x.Close()"
  will only match Close() calls on *os.File values, not on other types.

USE THIS TOOL WHEN: you want to find specific code patterns, API usages,
or structural idioms across a codebase. For control-flow properties
(resource leaks, missing error checks), use the specialized tools instead.`,
	InputSchema: mcpsdk.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "Go code pattern with metavariables. See tool description for syntax.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "File or directory to search. Defaults to current working directory.",
			},
			"type_constraints": map[string]any{
				"type":        "object",
				"description": "Map of metavariable name → Go type string. Only matches where the metavariable has the specified type. Example: {\"x\": \"*os.File\", \"err\": \"error\"}",
				"additionalProperties": map[string]any{"type": "string"},
			},
			"include_equivalences": map[string]any{
				"type":        "boolean",
				"description": "If true, also match semantically equivalent forms (e.g., 'x != nil' also matches '!(x == nil)'). Default: true.",
			},
		},
		Required: []string{"pattern"},
	},
}

var findTypeUsageTool = mcpsdk.Tool{
	Name: "find_type_usage",
	Description: `Find all usages of a specific Go type across the codebase. Returns where the
type is instantiated, passed as an argument, returned, stored in a field, or
used in a type assertion/switch.

USE THIS TOOL WHEN: you want to understand how a type is used before
refactoring it, or to find all places affected by a type change.`,
	InputSchema: mcpsdk.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"type_name": map[string]any{
				"type":        "string",
				"description": "Fully qualified type name. Examples: '*os.File', 'context.Context', 'io.Reader', 'mypackage.Config'",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "File or directory to search. Defaults to current working directory.",
			},
			"usage_kinds": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "string",
					"enum": []string{"instantiation", "argument", "return", "field", "type_assertion", "assignment", "all"},
				},
				"description": "Filter by usage kind. Default: [\"all\"]",
			},
		},
		Required: []string{"type_name"},
	},
}

// --------------------------------------------------------------------------
// 2. Control Flow Analysis — the 20% (CTL hidden inside)
// --------------------------------------------------------------------------

var findResourceLeakTool = mcpsdk.Tool{
	Name: "find_resource_leak",
	Description: `Find resources that are acquired but not properly released on all code paths.

This tool builds a control flow graph for each function and checks that
every acquisition of a resource is followed by a matching release on ALL
paths to function exit (including error returns and panics).

COMMON PATTERNS IT CATCHES:
  - os.Open() without defer f.Close()
  - sql.Query() without defer rows.Close()
  - sync.Mutex.Lock() without Unlock() on error paths
  - net.Dial() without conn.Close()

HOW IT WORKS INTERNALLY (you don't need to specify this):
  The tool compiles your acquire/release pair into a path property:
  "on ALL paths from acquire to function exit, release must occur."
  This is equivalent to the CTL formula AF(release) — but you never
  need to write that. Just specify what acquires and what releases.

USE THIS TOOL WHEN: you suspect resource leaks, or before/after refactoring
code that handles resources. Also useful for reviewing new code that opens
files, connections, or locks.`,
	InputSchema: mcpsdk.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"acquire_pattern": map[string]any{
				"type": "string",
				"description": `Pattern for the acquisition call. Uses the same syntax as find_pattern.
Examples:
  "os.Open($path)"          — matches os.Open
  "os.OpenFile($args...)"   — matches os.OpenFile
  "$db.Query($args...)"     — matches Query on any *sql.DB
  "$mu.Lock()"              — matches Lock on any sync.Mutex/RWMutex`,
			},
			"release_method": map[string]any{
				"type":        "string",
				"description": "Method name that releases the resource. Default: \"Close\". For mutexes use \"Unlock\".",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "File or directory to analyze. Defaults to current working directory.",
			},
			"severity": map[string]any{
				"type":        "string",
				"enum":        []string{"any_path", "all_paths"},
				"description": "\"all_paths\" (default): only report when release is missing on EVERY path (definite leak). \"any_path\": report when missing on ANY path (possible leak). Use \"any_path\" for stricter analysis.",
			},
		},
		Required: []string{"acquire_pattern"},
	},
}

var findUncheckedErrorTool = mcpsdk.Tool{
	Name: "find_unchecked_error",
	Description: `Find function calls that return an error which is never checked.

This tool performs control-flow-aware analysis: it doesn't just look for
"_ = foo()" — it also finds cases where an error is assigned to a variable
that is never checked before being overwritten or going out of scope.

WHAT IT CATCHES:
  - Explicitly ignored errors:     "_ = f.Close()"
  - Silently discarded errors:     "f.Close()" (return value dropped)
  - Checked-then-shadowed errors:  err assigned but overwritten before use
  - Errors in defer statements:    "defer f.Close()" (common, configurable)

USE THIS TOOL WHEN: auditing error handling, reviewing new code, or
enforcing a "no ignored errors" policy.`,
	InputSchema: mcpsdk.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "File or directory to analyze. Defaults to current working directory.",
			},
			"func_pattern": map[string]any{
				"type":        "string",
				"description": "Optional: only check calls matching this pattern. Example: \"$x.Close()\" to only check Close calls.",
			},
			"include_defers": map[string]any{
				"type":        "boolean",
				"description": "Whether to flag unchecked errors in defer statements. Default: false (deferred Close errors are commonly ignored intentionally).",
			},
			"include_explicit_blank": map[string]any{
				"type":        "boolean",
				"description": "Whether to flag explicit '_ = fn()' assignments. Default: true.",
			},
		},
	},
}

var findLockViolationTool = mcpsdk.Tool{
	Name: "find_lock_violation",
	Description: `Find lock/unlock discipline violations in concurrent code.

Checks that every Lock() has a matching Unlock() on all paths, detects
double-locks, and finds cases where a lock is held across a potentially
blocking operation.

WHAT IT CATCHES:
  - Lock() without Unlock() on error return paths
  - Double Lock() (deadlock)
  - Lock held across channel operations or I/O (potential deadlock)
  - RLock/RUnlock mixed with Lock/Unlock on the same mutex

USE THIS TOOL WHEN: reviewing concurrent code, investigating deadlocks,
or auditing mutex usage.`,
	InputSchema: mcpsdk.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "File or directory to analyze. Defaults to current working directory.",
			},
			"check_blocking_under_lock": map[string]any{
				"type":        "boolean",
				"description": "If true, also flag when a lock is held across potentially blocking operations (channel send/recv, I/O calls). Default: true.",
			},
		},
	},
}

var findNilDerefTool = mcpsdk.Tool{
	Name: "find_nil_deref",
	Description: `Find potential nil pointer dereferences by tracing pointer values through
the control flow graph.

Checks that pointers returned from functions that may return nil (indicated
by an accompanying error or ok boolean) are checked before being dereferenced.

WHAT IT CATCHES:
  - Using a pointer from a (value, error) return without checking error first
  - Using a value from a (value, ok) map/typeassert without checking ok
  - Dereferencing a pointer that was checked for nil on one path but not another

USE THIS TOOL WHEN: reviewing code that handles pointers, map lookups,
or type assertions.`,
	InputSchema: mcpsdk.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "File or directory to analyze. Defaults to current working directory.",
			},
			"func_pattern": map[string]any{
				"type":        "string",
				"description": "Optional: only analyze specific functions matching this pattern.",
			},
		},
	},
}

var checkPathPropertyTool = mcpsdk.Tool{
	Name: "check_path_property",
	Description: `Generic control-flow path property checker. This is the most powerful
analysis tool — it checks whether a given property holds on code paths
between two points.

This is the tool to use when the specialized tools (find_resource_leak,
find_lock_violation, etc.) don't cover your specific use case.

PROPERTY TYPES:
  "always_before"  — B must always occur before A on every path
  "always_after"   — A must always be followed by B on every path
  "never_between"  — C must never occur between A and B
  "exists_between" — C must occur at least once between A and B on some path
  "dominates"      — A must dominate B (A occurs on every path to B)

EXAMPLES:
  Check that validateInput() is always called before processRequest():
    from_pattern: "processRequest($args...)"
    property: "always_before"
    target_pattern: "validateInput($args...)"

  Check that a transaction is never committed without first checking errors:
    from_pattern: "$tx.Commit()"
    property: "always_before"
    target_pattern: "if $err != nil"

USE THIS TOOL WHEN: you need to verify an ordering or reachability property
between two code points that isn't covered by the specialized tools.`,
	InputSchema: mcpsdk.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"from_pattern": map[string]any{
				"type":        "string",
				"description": "Pattern for the starting point (same syntax as find_pattern).",
			},
			"target_pattern": map[string]any{
				"type":        "string",
				"description": "Pattern for the property target.",
			},
			"property": map[string]any{
				"type": "string",
				"enum": []string{"always_before", "always_after", "never_between", "exists_between", "dominates"},
				"description": `The property to check:
  "always_before"  — target must always precede from_pattern
  "always_after"   — target must always follow from_pattern
  "never_between"  — exclude_pattern must never appear between from and target
  "exists_between" — exclude_pattern must appear at least once between from and target
  "dominates"      — from_pattern must dominate target_pattern`,
			},
			"exclude_pattern": map[string]any{
				"type":        "string",
				"description": "For never_between/exists_between: the pattern that must not/must appear between from and target.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "File or directory to analyze. Defaults to current working directory.",
			},
			"quantifier": map[string]any{
				"type":        "string",
				"enum":        []string{"all_paths", "any_path"},
				"description": "\"all_paths\" (default): property must hold on every path. \"any_path\": property must hold on at least one path.",
			},
		},
		Required: []string{"from_pattern", "target_pattern", "property"},
	},
}

// --------------------------------------------------------------------------
// 3. Transformations
// --------------------------------------------------------------------------

var applyTransformTool = mcpsdk.Tool{
	Name: "apply_transform",
	Description: `Apply a structural code transformation. Matches a pattern and replaces it
with a new pattern, preserving metavariable bindings.

This operates at the AST level — it understands Go syntax, preserves
comments and formatting, and applies gofmt to the result.

SYNTAX:
  The match and replace patterns use the same syntax as find_pattern.
  Metavariables captured in the match are available in the replacement.

EXAMPLES:
  Rename a function:
    match:   "oldPkg.OldFunc($args...)"
    replace: "newPkg.NewFunc($args...)"

  Add context parameter:
    match:   "$recv.DoSomething($args...)"
    replace: "$recv.DoSomething(ctx, $args...)"
    (with type_constraints: {"recv": "*mypackage.Client"})

  Replace error wrapping style:
    match:   "fmt.Errorf(\"failed to %s: %v\", $desc, $err)"
    replace: "fmt.Errorf(\"failed to %s: %w\", $desc, $err)"

USE THIS TOOL WHEN: you need to make a consistent change to a code pattern
across the codebase. Always use preview mode first to verify the changes.`,
	InputSchema: mcpsdk.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"match": map[string]any{
				"type":        "string",
				"description": "Pattern to match (same syntax as find_pattern).",
			},
			"replace": map[string]any{
				"type":        "string",
				"description": "Replacement pattern. Use the same metavariable names as in match.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "File or directory to transform. Defaults to current working directory.",
			},
			"type_constraints": map[string]any{
				"type":        "object",
				"description": "Type constraints on metavariables (same as find_pattern).",
				"additionalProperties": map[string]any{"type": "string"},
			},
			"preview": map[string]any{
				"type":        "boolean",
				"description": "If true (default), show what would change without applying. Set to false to apply changes.",
			},
		},
		Required: []string{"match", "replace"},
	},
}

var migrateAPITool = mcpsdk.Tool{
	Name: "migrate_api",
	Description: `Migrate from one API to another across the entire codebase. This is a
higher-level tool than apply_transform — it handles:

  - Function/method renames
  - Parameter reordering, addition, removal
  - Return value changes
  - Import path updates
  - Chained transformations (multiple rules applied in sequence)

EXAMPLES:
  Migrate from io/ioutil to io+os (Go 1.16+):
    rules: [
      {"old": "ioutil.ReadAll($r)", "new": "io.ReadAll($r)"},
      {"old": "ioutil.ReadFile($f)", "new": "os.ReadFile($f)"},
      {"old": "ioutil.WriteFile($f, $d, $p)", "new": "os.WriteFile($f, $d, $p)"}
    ]
    update_imports: true

USE THIS TOOL WHEN: doing API migrations, library upgrades, or large-scale
refactoring that involves changing how functions are called.`,
	InputSchema: mcpsdk.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"rules": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"old": map[string]any{
							"type":        "string",
							"description": "Pattern for the old API call.",
						},
						"new": map[string]any{
							"type":        "string",
							"description": "Replacement pattern for the new API call.",
						},
						"type_constraints": map[string]any{
							"type":                 "object",
							"additionalProperties": map[string]any{"type": "string"},
						},
					},
					"required": []string{"old", "new"},
				},
				"description": "List of transformation rules to apply in sequence.",
			},
			"update_imports": map[string]any{
				"type":        "boolean",
				"description": "Automatically add new imports and remove unused old imports. Default: true.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "File or directory to transform. Defaults to current working directory.",
			},
			"preview": map[string]any{
				"type":        "boolean",
				"description": "If true (default), show what would change without applying.",
			},
		},
		Required: []string{"rules"},
	},
}

var addMissingCleanupTool = mcpsdk.Tool{
	Name: "add_missing_cleanup",
	Description: `Automatically insert missing cleanup/defer statements for resources.

This combines find_resource_leak with a transformation: for each detected
leak, it inserts a 'defer resource.Release()' immediately after the
acquisition, with proper error checking.

WHAT IT GENERATES:
  For "f, err := os.Open(path)" without defer:
    → inserts "defer f.Close()" after the error check

  For "$mu.Lock()" without Unlock on error path:
    → inserts "defer $mu.Unlock()" or wraps the error path

USE THIS TOOL WHEN: you've found resource leaks with find_resource_leak
and want to auto-fix them. Always preview first.`,
	InputSchema: mcpsdk.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"acquire_pattern": map[string]any{
				"type":        "string",
				"description": "Pattern for the acquisition (same as find_resource_leak).",
			},
			"release_method": map[string]any{
				"type":        "string",
				"description": "Method to call for cleanup. Default: \"Close\".",
			},
			"cleanup_style": map[string]any{
				"type":        "string",
				"enum":        []string{"defer", "explicit"},
				"description": "\"defer\" (default): insert defer after acquisition. \"explicit\": insert explicit call before each return.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "File or directory. Defaults to current working directory.",
			},
			"preview": map[string]any{
				"type":        "boolean",
				"description": "If true (default), show what would change without applying.",
			},
		},
		Required: []string{"acquire_pattern"},
	},
}

// --------------------------------------------------------------------------
// 4. Inspection & Validation
// --------------------------------------------------------------------------

var explainMatchTool = mcpsdk.Tool{
	Name: "explain_match",
	Description: `Explain WHY a pattern matches (or doesn't match) at a specific location.

Returns a step-by-step breakdown of how each part of the pattern was
matched against the code, including metavariable bindings, type resolution,
and which equivalences (isomorphisms) were applied.

USE THIS TOOL WHEN: a find_pattern or analysis result is surprising and
you want to understand the matching logic, or when debugging why an
expected match wasn't found.`,
	InputSchema: mcpsdk.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "The pattern to explain matching for.",
			},
			"file": map[string]any{
				"type":        "string",
				"description": "The file containing the code to match against.",
			},
			"line": map[string]any{
				"type":        "integer",
				"description": "The line number to examine.",
			},
		},
		Required: []string{"pattern", "file", "line"},
	},
}

var listEquivalencesTool = mcpsdk.Tool{
	Name: "list_equivalences",
	Description: `List all active Go equivalences (isomorphisms) that affect pattern matching.

Go equivalences automatically expand patterns to match semantically
equivalent code. For example, the pattern 'len(s) == 0' also matches
's == nil' for slice types because these are semantically equivalent.

Returns each equivalence with its name, description, and examples.

USE THIS TOOL WHEN: you want to understand what additional forms a pattern
might match, or to debug unexpected matches from equivalence expansion.`,
	InputSchema: mcpsdk.ToolInputSchema{
		Type: "object",
		Properties: map[string]any{
			"category": map[string]any{
				"type":        "string",
				"enum":        []string{"nil_checks", "error_patterns", "boolean_logic", "comparison", "slice_map", "all"},
				"description": "Filter equivalences by category. Default: \"all\".",
			},
		},
	},
}

// allTools returns all tools the MCP server exposes.
func allTools() []mcpsdk.Tool {
	return []mcpsdk.Tool{
		// Pattern matching.
		findPatternTool,
		findTypeUsageTool,

		// Control flow analysis.
		findResourceLeakTool,
		findUncheckedErrorTool,
		findLockViolationTool,
		findNilDerefTool,
		checkPathPropertyTool,

		// Transformations.
		applyTransformTool,
		migrateAPITool,
		addMissingCleanupTool,

		// Inspection.
		explainMatchTool,
		listEquivalencesTool,
	}
}
