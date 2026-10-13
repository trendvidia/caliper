// SPDX-License-Identifier: MIT
// Copyright (c) 2026 TrendVidia, LLC.

// Package iso defines Go-specific isomorphisms (equivalences) that expand
// patterns to match semantically equivalent code forms.
//
// This is the Go equivalent of Coccinelle's isomorphism system. Each
// equivalence describes two or more code forms that are semantically
// identical, allowing a single pattern to match all of them.
package iso
