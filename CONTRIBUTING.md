# Contributing

## Gate

`make verify` is the per-change gate — it must stay green:

```
make verify   # build + vet + lint (golangci-lint v2) + test + fixture gate + gofmt
```

CI additionally runs the suite under `-race` and splits into the two gate jobs
(currency + snapshot). Run `go test -race ./...` locally before pushing changes
that touch capture/await/canon/harness.

## Go version floor (read before `go get`)

The **engine module keeps a Go 1.24 floor** (`go.mod: go 1.24.0`) — that is the
version adopters build against, and it must build standalone (`GOWORK=off go
build ./...`). The **workspace stays at 1.25** (`go.work: go 1.25.0`) because a
few fixture modules declare `go 1.25.0` (e.g. `testdata/fixtures/{reflectsvc,
impeachsvc,reclaimsvc,dispatchsvc}`) and the workspace `go` directive must be at
least the highest of its member modules.

`go get`-ing a new dependency can silently bump the `go` directive to match the
local toolchain. After adding a dependency:

1. Keep the engine's deps 1.24-compatible (pin if a transitive dep demands 1.25).
2. Restore `go.mod`/`go.work` `go` directives if `go get` bumped them
   (engine → `1.24.0`, workspace → `1.25.0`).
3. Verify both: `GOWORK=off go build ./...` (engine at 1.24) and
   `go build ./...` (workspace at 1.25).

## Schema versions

The gated artifacts carry a `schema_version` (`flowmap.boundary/v1`,
`flowmap.trace/v1`). Changing a canonical form is a coordinated regeneration:
bump the version, then regenerate every committed artifact (`flowmap boundary`,
`go test -update`) in the same change. The version is part of snapshot equality,
so a bump deliberately fails stale goldens.
