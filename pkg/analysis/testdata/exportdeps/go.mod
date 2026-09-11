module exportdeps

go 1.23

require (
	github.com/google/uuid v1.6.0
	local.example/nocompile v0.0.0
)

replace local.example/nocompile => ./nocompile
