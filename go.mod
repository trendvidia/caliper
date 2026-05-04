module github.com/trendvidia/caliper

go 1.26.1

require (
	github.com/mark3labs/mcp-go v0.28.0
	github.com/trendvidia/protowire-go v0.0.0-00010101000000-000000000000
	golang.org/x/tools v0.29.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/spf13/cast v1.7.1 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/mod v0.22.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
)

// Use the trendvidia fork until panic-recovery and other patches are
// upstreamed. Layout: ../mcp-go alongside this repo.
replace github.com/mark3labs/mcp-go => ../mcp-go

replace github.com/trendvidia/protowire-go => ../protowire-go

// protowire-go uses dynamicpb.Message.SetUnsafe (a fork-only fast-path).
// Pull in the same protobuf-go fork it builds against; keeps the
// vendored copy compilable here.
replace google.golang.org/protobuf => ../protobuf-go
