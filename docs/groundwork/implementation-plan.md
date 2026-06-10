# groundwork: implementation plan

This is the plan-of-record for building `groundwork` on top of flowmap. It is
grounded in flowmap's *actual* graph output (verified against the
`testdata/fixtures/loansvc` fixture, not just the design docs) and folds in the
corrections from the plan's own pressure test (last section).

Read the design record first: [`distilled-learnings.md`](distilled-learnings.md),
[`mr-review-artifacts.md`](mr-review-artifacts.md),
[`pressure-test.md`](pressure-test.md).

---

## 1. The interface (verified facts, no flowmap changes needed)

`groundwork` consumes two artifacts flowmap already emits as canonical JSON:

- **`flowmap graph <dir>`** → the call-graph view:
  - `nodes[]`: `{fqn, sig, tier, fallible}`
  - `edges[]`: `{from, to, tier, boundary?, concurrent?}` where
    `boundary ∈ {outbound-sync, outbound-async, inbound}` and `to` is either an
    FQN or a typed boundary node — `boundary:bus PUBLISH loan.approved`,
    `boundary:db INSERT ledger`, `boundary:bus PUBLISH <dynamic>`.
  - `blind_spots[]`: `{kind, site}`,
    `kind ∈ {NonConstantBoundaryArg, UnresolvedDispatch, reflect, HighFanOut, unsafe, cgo, go:linkname}`.
- **`flowmap boundary <dir>`** → the gated contract:
  `{service, entrypoints{http,consumers}, published, consumed, external_dependencies, blind_spots}`.

Three properties — already true today — make this the right substrate:

1. **Deterministic + canonical.** Everything routes through `canonjson.Marshal`
   (sorted, stable); verified byte-identical across repeat runs. That is what
   makes a content digest meaningful.
2. **Blind-spot honesty is in-band.** `<dynamic>` markers and the blind-spot
   manifest are emitted, so `groundwork` can compute the `trust: reliable|verify`
   flag rather than guess.
3. **The trust boundary is enforceable at the file level** — `groundwork` reads
   graph *files* and never runs flowmap, so "graph from trusted CI, never the
   agent" becomes a CI-wiring rule (see §5, corrected).

## 2. Placement

A **separate binary** `cmd/groundwork` in this module, reading flowmap's JSON
outputs as files. Keeping the producer (flowmap) and the judge (groundwork) as
different programs is what lets CI control which runs where. It inherits
flowmap's existing philosophy: human-as-oracle, **no AI in the verdict path** —
every groundwork output is a pure function of (graph, policy, delta).

## 3. Structure

```
cmd/groundwork/         CLI: impact | ground | fitness | verify | diff | review | verify-artifact
internal/groundwork/
  graph/                load + index flowmap graph JSON (forward/reverse reachability, scope)
  policy/               policy.json schema: invariants, allow-lists, budgets, layer-map
  fitness/              deterministic invariant checks (layering, must-not-reach, IO budget)
  impact/               blast radius (transitive callers/callees, entrypoint cover, risk score)
  ground/               per-function grounding card + trust flag
  contract/             boundary-contract diff (breaking/additive)
  review/               base-vs-branch artifact + sha256 digest
  verify/               pre-flight delta gate
docs/groundwork/        this design record
testdata/groundwork/    fixtures (see §6 — loansvc alone is NOT sufficient)
```

The unifying primitive is the **fitness function**:
`func(GraphIndex, Policy) []Violation`, each violation naming an exact edge or
symbol. `verify` and `review` both run the fitness set on a delta/branch and
report only *new* violations.

## 4. Build order

Sequenced by the design record's own finding that the value is the unique
quadrant (drift ratchet + all-paths invariants) and the integrity model — not
breadth.

- **Phase 0 — Core.** Graph loader + bidirectional reachability index +
  `policy.json` schema. No surface yet.
  *Exit: load the fixture graph; answer "who reaches X / what does X reach" correctly.*
- **Phase 1 — `fitness` (the drift ratchet).** Layering (allow-listed),
  `must_not_reach` reachability invariants, per-route I/O budget. Lessons #1/#8.
  *Exit: a policy that passes on the strictly-layered fixture and fails on a skip-level edge.*
- **Phase 2 — `review` + `verify-artifact` (the integrity model).**
  Base-vs-branch artifact with the **three-valued verdict**
  (`BLOCK` / `STRUCTURALLY-CLEAR` / `NO-STRUCTURAL-SIGNAL`) from day one,
  sha256 digest, and recompute-from-source verification.
  *Exit: the "publisher health endpoint" demo — same description, BLOCK vs CLEAR.*
- **Phase 3 — `verify` + `diff`.** *Done.* `review.Gate` is the fail-closed
  pre-flight gate: it blocks on a new violation, a touched package outside the
  declared `--scope` (scope creep, computed from the same delta the review uses),
  or a breaking contract change, and carries a reproducibility digest. The new
  `contract` package decodes flowmap's boundary contract and `Compare` flags
  breaking inter-service movement (removed route / published / consumed = breaking;
  additions and dependency changes = informational). CLI: `groundwork verify
  <policy> <base> <branch> [--scope p,q] [--json]` and `groundwork diff
  <base-contract> <branch-contract>`, both fail-closed. Verified end-to-end on the
  fixtures.
- **Phase 4 — zero-touch CI (the "open next step").** A CODEOWNERS-gated CI job
  that regenerates *both* base and branch graphs from checked-out source and
  feeds them to `review`/`verify`. This is the trust anchor (§5), not polish.
- **Phase 5 — `impact` + `ground` (agent-facing).** Blast-radius and grounding
  cards. Useful but partly displaced by the agent's own loop, so last.
- **Deferred — mode-2 value-flow** (`<dynamic>` topic resolution via
  `awsnaming.*` provenance). Shelved unless routing bugs slip past e2e.

Phases 0–2 are the load-bearing core and deliver the unique value on their own.

## 5. Corrections baked in (from the plan pressure test)

These four are folded into the plan above before any code:

1. **The trust anchor is the CI job, not the binary split.** A separate binary is
   necessary but not sufficient. What enforces "graph from trusted CI, never the
   agent" is a **CODEOWNERS-gated CI job that regenerates both graphs from
   checked-out branch source every run and never reads a committed graph
   artifact** — reusing flowmap's existing currency-gate discipline.
2. **`policy.json` is a CODEOWNERS-gated artifact.** Layering needs a
   human-authored package→layer map; if the agent under review authors the
   policy, it grades its own homework (lesson #7). The policy is reviewed by a
   human exactly like the boundary contract.
3. **Every "absence" verdict is three-valued.** `must_not_reach` and
   contract-clear must distinguish `PROVEN-ABSENT` (no path *and* no blind spot in
   the reachable frontier) from `NO-PATH-FOUND` (no path but N blind spots → not a
   proof) from `REACHABLE`. A silent green over a blind-spot frontier repeats the
   Attack-4 mistake one level down. `NO-STRUCTURAL-SIGNAL` is **silent by
   default** (exit 0, no broadcast) so it doesn't get muted — it surfaces only
   paired with a positive routing fact.
4. **Graphs are generated as a pinned, back-to-back base+branch pair** in one CI
   run. Determinism holds within a toolchain (verified) but SSA artifacts
   (anonymous-closure numbering `$1`/`$2`, devirtualization) can shift across Go
   versions, producing phantom deltas → false BLOCKs. The trust anchor and the
   determinism anchor are the same job.

## 6. Fixtures — loansvc alone is NOT sufficient

Two cracks found by running on the real fixture:

- **loansvc is not strictly layered.** Its base already contains
  `handler.App.Status → store.Loans.SelectLoan` — a handler→store edge. So the
  "skip-level → BLOCK" demo cannot run on loansvc honestly; a "no handler→store"
  invariant already fails on base. **Add a minimal strictly-`handler → app → store`
  fixture** so a skip-level edge is genuinely novel.
- **loansvc has zero blind spots** (`blind_spots: []`). It therefore cannot test
  the most important property — that fitness correctly *abstains* in blind-spot
  regions (correction #3). **Add a fixture that produces blind spots** (an
  interface registry → `UnresolvedDispatch`, a fan-out hub → `HighFanOut`).

loansvc remains useful for the boundary/contract and reachability surfaces; it is
necessary, not sufficient.

## 7. The boundary (unchanged)

groundwork certifies **structure** — dependencies, reachability, side-effect
surface, contract. It does **not** verify logic inside a function (that stays
tests, types, and the existing Go analyzers). Its single point of failure is the
integrity of the graph it is handed, which must come from trusted CI (§5.1),
never from the agent under review.

## 8. Progress

**Phase 0 — done.** Shipped:
- `internal/groundwork/graph` — decoupled decode of flowmap's graph JSON, plus an
  `Index` with bidirectional transitive reachability (`Reachable`/`Reaching`),
  `Sources`/`EntrypointCover`, boundary-`Effects` collection, and blind-spot
  lookup. Strict decode (`DisallowUnknownFields`) so a flowmap schema change fails
  loudly.
- `internal/groundwork/policy` — the CODEOWNERS-gated policy schema (layers,
  layering allow-list + roots, must-not-reach, I/O budget) with strict load and
  validation, plus `LayerOf` (longest-prefix wins) / `LayerRank`.
- `cmd/groundwork` — `reach` and `policy-check` introspection surfaces (no verdict
  yet; those arrive with `fitness`).

**Both fixtures — done**, added to `go.work`, with committed graph goldens under
`testdata/groundwork/goldens` (regenerate via `testdata/groundwork/regen.sh`) and
a sample policy under `testdata/groundwork/policies`:
- `layeredsvc` — strict `handler → app → store`, **no** handler→store edge on
  base (verified), so a skip-level edge is genuinely novel; `UpdateProfile` does
  two DB writes so the I/O budget has material.
- `blindsvc` — produces a `reflect` and an `unsafe` graph blind spot, a
  `boundary:bus PUBLISH <dynamic>` edge (the `Publish` route reaches it; the
  `Create` route reaches only a *named* publish — the clean must-not-reach
  contrast).

Two facts the build surfaced, worth carrying into Phase 1:
- **Blind spots live in two places.** The graph JSON's `blind_spots` array carries
  only the *graph-completeness* subset (reflect / HighFanOut / unsafe / cgo /
  linkname). The *boundary* blind spots (dynamic publish/dispatch) ride the
  boundary contract and surface in the graph **as a `<dynamic>` edge target**. So
  groundwork's `trust: verify` / three-valued reachability must consult the
  `<dynamic>` edge markers *and* the graph blind-spot manifest (and, in Phase 3,
  the contract) — not the manifest alone.
- **Entrypoints aren't labelled in the graph.** `Sources()` derives them
  structurally as in-degree-0 nodes (mains, dynamically-dispatched handlers,
  exports). This is the graph-only approximation; the boundary contract can later
  attach route/topic names.

**Phase 1 — done.** `internal/groundwork/fitness` evaluates a policy against a
graph index into deterministic findings, each naming the exact edge/symbol:
- **layering** — a call may stay within a layer or descend one; skip-level and
  upward calls are violations, except out of a declared root package or an
  allow-listed edge. Only edges with *both* endpoints in declared layers are
  judged. Verified: passes on `layeredsvc` base, fails naming
  `handler → store skips 1 layer(s)` when a skip edge is introduced.
- **must_not_reach** — three-valued, exactly as the pressure test demanded:
  `PROVEN-ABSENT` (no path, frontier fully resolved → silent pass),
  `NO-PATH-FOUND` (no path but a blind frontier → a **caution**, exit 0, naming
  where the graph went blind), `REACHABLE` (a path → violation). All three are
  demonstrated on the committed fixtures (`layeredsvc` proves a read route stays
  read-only; `blindsvc`'s create route raises the caution at `reflect at
  encode.Marshal`; its publish route is a reachable violation through the
  `<dynamic>` edge).
- **io_budget** — caps reachable *writes* per entrypoint (DB mutations, bus
  PUBLISH, outbound non-GET); reads do not count. Verified: budget 2 passes on
  `layeredsvc`, budget 1 fails naming the UpdateUser route's two writes.

CLI: `groundwork fitness <policy> <graph>` (non-zero exit on violation; cautions
print but do not fail). `make verify` green.

**Phase 2 — done.** `internal/groundwork/review` computes the base-vs-branch MR
artifact as a pure function of (policy, base graph, branch graph):
- **three-valued verdict** baked in from day one: `NO-STRUCTURAL-SIGNAL` (graphs
  identical → body-only, the graph abstains and says so), `STRUCTURALLY-CLEAR`
  (structure changed, no invariant broke), `BLOCK` (a new violation or a breaking
  contract change). Reports **only newly-introduced** violations (branch findings
  minus base findings), tested.
- **shape** (body-only / localized / cross-package / broad) + per-package node
  `Touches`; **contract movement** (entrypoints + bus/outbound, additive vs
  breaking; DB excluded as the service's own store); **I/O effects** (incl. DB
  writes); **reach** (existing entrypoints the change is now live behind).
- **digest + verify-artifact** — `sha256` over the canonical artifact;
  `VerifyArtifact` runs the two pressure-test checks: body integrity (→ TAMPERED)
  and recompute-from-trusted-graphs (→ STALE). The re-signed forgery (edit body
  *and* re-sign) is caught as STALE by recomputation — the digest is explicitly
  *not* the anchor.

Headline demo, on the committed fixtures, verified end-to-end: the same
"add GetUserFast read endpoint" feature renders **STRUCTURALLY-CLEAR** wired
`handler→app` and **BLOCK** (naming the exact skip edge) wired `handler→store` —
same description, different *computed* verdict. Branch goldens are derived from
the real `layeredsvc` base by one documented edge (see `regen.sh`).

CLI: `groundwork review <policy> <base> <branch> [--json]` (BLOCK exits non-zero)
and `groundwork verify-artifact <artifact> <policy> <base> <branch>`.

**Post-Phase-2 review fixes** (from the max-effort `/code-review`):
- **Layering judges *effective* edges, not just direct ones.** A skip smuggled
  through an unassigned helper package (`handler → codec → store`) previously
  evaded the gate entirely; now layering follows paths through non-layer packages
  and stops at the first layer (the legitimate `handler→app→store` spine is
  absorbed by `app`), so the bounce surfaces as a `handler → store` skip.
- **I/O budget excludes the composition root.** `main`'s startup writes
  (migrations/seeding) are no longer charged against a per-route budget; only
  non-root entrypoints are judged (the boundary contract refines "route" further
  in Phase 3).
- **`must_not_reach` gained `require_proof`.** A high-stakes safety rule can now
  fail closed: when the frontier is blind, the default is a non-blocking caution,
  but a `require_proof` rule escalates that unprovability to a Violation.

Follow-up cleanup (review findings #4–#8): a boundary-label *contract test*
(`review`) pins flowmap's effect-label format that `IsWrite`/`classifyContract`
parse (with a committed `loansvc` graph golden for full vocabulary coverage);
`graph.Index` now precomputes sorted/deduped adjacency once instead of re-sorting
on every reachability walk; a shared `setutil` package replaces the duplicated
set helpers; `pkgDeltas` gained the empty-package guard; and `policy.Load` decodes
bytes directly.

Next: Phase 3 (`verify` + `diff`) — pre-flight delta gate and boundary-contract
diff; then Phase 4 (zero-touch CI: the trusted base+branch graph generation).
