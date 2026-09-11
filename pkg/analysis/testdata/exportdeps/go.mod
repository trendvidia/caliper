module exportdeps

go 1.23

require (
	github.com/google/uuid v1.6.0
	local.example/bodyerr v0.0.0
	local.example/declerr v0.0.0
	local.example/nocompile v0.0.0
)

replace (
	local.example/bodyerr => ./bodyerr
	local.example/declerr => ./declerr
	local.example/nocompile => ./nocompile
)
