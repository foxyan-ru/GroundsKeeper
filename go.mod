module github.com/mstone/groundskeeper

go 1.24

// NOTE: `go mod tidy` has never been run against this module and there is no
// go.sum committed alongside it. The versions below are plausible, recent
// pins for each dependency chosen by hand while scaffolding this project;
// they have not been verified to resolve or build. Before this module is
// ever built, run `go mod tidy` to generate a real go.sum and correct any
// version drift.
require (
	github.com/docker/docker v26.1.4+incompatible
	github.com/google/go-github/v66 v66.0.0
	github.com/google/uuid v1.6.0
	gopkg.in/yaml.v3 v3.0.1
)
