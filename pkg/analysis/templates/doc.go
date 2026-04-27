// Package templates provides pre-built analysis templates that encapsulate
// complex control-flow properties behind simple, parameterized interfaces.
//
// This is how Caliper hides CTL complexity from the LLM. Each template
// knows which path quantifier to use, how to build predicates, and how to
// format results. The MCP tools call these directly.
package templates
