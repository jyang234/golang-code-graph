# Idea: harden the static-analysis front-end against silent regressions

> **`ACTIVE`** · deferred testing investment, not yet scheduled · _reviewed 2026-06-16_

**Status:** captured from a regression-hygiene assessment (2026-06-16) of the
production codebase. The near-term, additive safety-net items from that
assessment — the golden drift gate, the content-digest ratchet, loader fuzzing,
and a local gofmt gate — have shipped. This document holds the two items that
were deliberately deferred because they are larger, evidence-/understanding-gated
efforts rather than additive guards. Pick them up as a focused testing PR.

Related reading: the three-leg verification model in
[`path-obligations.md`](./path-obligations.md) (the "buy" leg is the generic
analyzers we lean on; this idea is about trusting the inputs to the "build" leg).

---

## Why the front-end is the soft underbelly

groundwork's verdict engines (`fitness`, `review`, `policy`, `graph`,
`obligations`, `frontier`) are the best-covered code in the repo (81–97%), which
is the right priority — they decide PASS/BLOCK. But every one of those verdicts
is only as sound as the **graph it is handed**, and that graph is built by the
static-analysis front-end:

| Package | Coverage (2026-06-16) | Role |
|---|---|---|
| `internal/static/signatures` | ~50% | reads function signatures off SSA |
| `internal/static/loader` | ~56% | loads packages, collects load errors, resolves the module |
| `internal/static/analyze` | ~61% | discovers registrars / entrypoints |

The weakest spots are the **error and edge paths** that shape the graph quietly:
`signatures.Of` (~40%), `loader.collectErrors` (~32%), `loader.moduleOf` (~56%),
`analyze.Registrars` (~47%). A bug here does not crash — it emits a *subtly
different graph*, and every downstream verdict then faithfully reasons over the
wrong substrate. That is the highest-trust-risk class for a tool whose whole
value is "the graph is true."

## Item 1 — raise front-end coverage on the error/edge paths

Not coverage-chasing for a number. The goal is to pin the *behavior* of the
paths that decide what makes it into the graph:

- `loader.collectErrors` / `loader.moduleOf`: a package that fails to load, a
  missing/odd `go.mod`, a partial build — does the loader fail loudly, or emit a
  silently-truncated package set the graph is then built from? Add fixtures that
  exercise the failure modes and assert the loader's contract (fail vs.
  documented-degrade), not just that a line ran.
- `signatures.Of`: generic instantiations, variadics, embedded/promoted methods,
  unnamed results — the shapes most likely to be mis-read.
- `analyze.Registrars`: routers/registration idioms that the discovery walk does
  or does not recognize (this directly determines the entrypoint set every
  reachability verdict starts from).

Then add a **non-regressing coverage floor** to CI (e.g. assert total
`>= 82%`, just under the current 83.4%) so the front-end cannot silently erode
while the engines stay green.

**Why deferred:** writing *meaningful* tests here requires understanding the
under-tested code first; a quick pass would be theater. Medium effort, its own PR.

## Item 2 — a determinism test at the front-end layer

The tool's central promise is deterministic, reproducible output, and that
promise is well-defended at the *output* layer (run-twice byte-equality on the
graph, per-engine digest stability). But the front-end builds maps of
packages / registrars / signatures, and their iteration order is currently only
*indirectly* guarded — a map-order leak in `loader`/`analyze`/`signatures` is
caught only if it happens to perturb the downstream `graphio` graph bytes.

Add a direct run-twice equality test at the `loader`/`analyze` layer: build the
intermediate representation twice from the same input and assert structural (or
byte) equality, so an order leak fails at its source with a precise signal
instead of as a confusing downstream golden diff.

**Why deferred:** low effort but most valuable paired with Item 1 (same fixtures,
same package focus); bundle them.

## Acceptance

- A `loader`/`analyze`/`signatures` test suite that exercises the named error
  and edge paths and asserts the loader's load-failure contract.
- A CI coverage floor that fails below ~82% total.
- A front-end run-twice determinism test.
