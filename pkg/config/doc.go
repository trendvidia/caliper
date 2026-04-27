// Package config handles loading and applying .caliper.pxf configuration.
//
// The config file lets users customize severity levels, enable/disable
// specific analysis rules, set default paths, and configure engine options
// without passing them on every MCP tool call. Configuration is stored in
// PXF format (Proto eXpressive Format) backed by protobuf schemas.
package config
