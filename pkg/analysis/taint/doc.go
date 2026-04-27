// Package taint implements inter-procedural taint analysis over SSA.
//
// Given a set of sources (where untrusted data enters), sinks (where
// untrusted data must not reach), and sanitizers (functions that cleanse
// data), the Tracker propagates taint through SSA values and the call
// graph, reporting any unsanitized source-to-sink flows.
package taint
