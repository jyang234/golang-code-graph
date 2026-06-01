# flowmap

A dual-pipeline PR-verification system for Go microservices. A human (via
CODEOWNERS) is the oracle — there is no AI in the verdict path.

- **Static pipeline** — from a service's source, build a call graph
  (`go/packages` → `go/ssa` → `go/callgraph`) and derive a *gated* inter-service
  **boundary contract** (published/consumed events, external HTTP/RPC
  dependencies, entry points) plus a **blind-spot manifest**. The full call graph
  + signatures are a *generated, non-gated* view. Gate = currency (regenerate,
  `git diff --exit-code`).
- **Behavioral pipeline** — service authors write in-process flow tests against
  flowmap; it captures OTel traces, canonicalizes them into a deterministic IR,
  commits **golden snapshots**, and gates via snapshot-assertion (`-update` to
  rebase).

Both pipelines share a **tier-map classifier** (features → tier 1–4) and a strict
**determinism discipline** (sort everything; canonical JSON). A **structural
diff** turns IR changes into a prioritized change set, and a **coverage-delta**
surfaces boundary effects no test exercises.

## Status

Under construction, phase by phase. See:

- `docs/` — the seven component specifications (the source of truth).
- `docs`-aligned `example-loan-svc-artifacts.md` — a worked example of every
  artifact flowmap emits.
- The implementation plan (phased, with per-phase verification).

## Layout

```
cmd/flowmap/   CLI: boundary [--check] | graph --entry | diff a b | coverage [--flows D] | version
harness/       PUBLIC: in-process capture harness for flow tests
flow/          PUBLIC: the flow test DSL
ir/            PUBLIC: the authoritative canonical IR (golden file shape)
internal/      the analysis engine (canonjson, glob, model, tiermap, static/, canon/, diff/, coverage/, …)
testdata/      hermetic fixture service (its own module) + committed goldens
```

## Adopting flowmap

`docs/adopting-flowmap.md` is the end-to-end recipe for wiring flowmap into a
service (instrument with OTel → commit the boundary contract → write a flow test
→ check coverage → wire CI/CODEOWNERS). The `testdata/fixtures/loansvc` fixture
is a complete worked example.

## The two gates (and how to keep them green)

flowmap has **two distinct gate mechanisms**, unified only by CODEOWNERS routing
and the human-as-oracle:

- **Currency gate (static).** The boundary contract is a pure function of the
  code, so CI regenerates it and fails on drift:
  `flowmap boundary --check <service-dir>`. Author workflow: after a boundary
  change, run `flowmap boundary <service-dir>` and commit the updated
  `.flowmap/boundary-contract.json` alongside the code.
- **Snapshot-assertion gate (behavioral).** The goldens are a function of
  *running* the code, so they ride `go test`; a stale golden fails the suite.
  Author workflow: after an intended behavior change, re-run the flow test with
  `-update` to rebase `*.golden.json` + `*.flow.md`, and commit them.

CODEOWNERS routes both gated artifacts, `.flowmap.yaml`, and the per-flow test
files to a human reviewer — a golden/contract change is unbypassable.

## Coverage

`flowmap coverage --flows <goldens-dir> <service-dir>` reports the boundary
effects (published/consumed events, external dependencies) that no committed
flow exercises — the delta between the static boundary (all reachable effects)
and the union of behavioral snapshots (tested effects). It is informational, not
a gate: a gap means "write a flow for this," not "fail the build."

## Schema versioning & regeneration

Gated artifacts carry a schema version (e.g. the boundary contract's
`flowmap.boundary/v1`). When flowmap changes a canonical form, it bumps the
version and **all adopting services must regenerate** the affected artifacts
(`flowmap boundary` for contracts, `go test -update` for goldens) in a
coordinated change — the real blast radius, made explicit rather than silent.

## Develop

```
make verify    # build + vet + lint + test + fixture gate + gofmt (the per-phase gate)
```
