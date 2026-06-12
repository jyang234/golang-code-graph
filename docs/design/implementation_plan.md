# flowmap (golang-code-graph) — Phased Implementation Plan

**Status:** historical — the original flowmap design record. Phases 0–8 (the
v1 core) are fully shipped. Of the deferred surface, Phase 10 (post-hoc
capture) shipped via [`post-hoc-behavioral-ingestion.md`](post-hoc-behavioral-ingestion.md);
Phases 9, 11–13 remain unbuilt. Kept for the architecture rationale and
decisions D1–D8; for the consumer layer built on top of flowmap's graph, see
[`../groundwork/implementation-plan.md`](../groundwork/implementation-plan.md)
and [`../groundwork/usage.md`](../groundwork/usage.md).

## Context

`/Users/johnyang/code/golang-code-graph` is a **greenfield** repo: seven specs in `docs/` and zero code. They describe **flowmap**, a **dual-pipeline PR-verification system for Go microservices**, with a human (via CODEOWNERS) as the oracle — **no AI in the verdict path**.

1. **Static pipeline** — from a service's source, build a call graph (`go/packages` → `go/ssa` → `go/callgraph`) and derive a *gated* **inter-service boundary contract** (published/consumed events, external HTTP/RPC deps, entry points) + a **blind-spot manifest**. Also a *non-gated* full call graph + signatures. Gate = **currency** (regenerate, `git diff --exit-code`).
2. **Behavioral pipeline** — service authors write in-process flow tests against flowmap; it captures OTel traces, **canonicalizes** them to a deterministic IR, commits **golden snapshots**, gates via **snapshot-assertion** (`-update` to rebase).

Shared: a **tier-map classifier** (features → tier 1–4), a strict **determinism discipline** (sort everything; canonical JSON), a **structural diff** (prioritized change set), and a **coverage-delta** (boundary effects no flow exercises).

**Oracles for this plan:** the seven `docs/` specs **and** `docs`-aligned `Downloads/example-loan-svc-artifacts.md` — a hand-built example of *every* artifact flowmap emits (`.flowmap/boundary-contract.json`, per-flow `*.golden.json` + `*.flow.md`, the `flow`/`harness` test DSL, coverage output, `.flowmap.yaml`). The example file drove the pressure test below and is the shape the acceptance tests target.

---

## Decisions (confirmed)

| # | Decision | **Choice** | Notes |
|---|----------|-----------|-------|
| D1 | **Scope** | **Full system, incl. the spec's deferred items** | Phases 0–8 = v1 core (in-process, single-service); Phases 9–13 = error-path/fault-injection, post-hoc capture, inter-service E2E, Pact-style contract tests, queryable interface + cross-repo composition. |
| D2 | **Build order** | **Static first** after the shared core | Pure function of source, no harness needed, fastest to a gated artifact. |
| D3 | **Module path** | `github.com/jyang234/golang-code-graph` | **Binary/CLI + config dir = `flowmap`** (from the artifacts: `flowmap graph`, `.flowmap/`, `.flowmap.yaml`). Module path and product name are distinct and both honored. |
| D4 | **Test corpus** | **Toy fixture + a real repo** | Toy fixture service (its own module) for hermetic golden tests; a real instrumented Go repo (you provide the path) as a non-blocking smoke/lighthouse target. |
| D5 | **CLI framework** | stdlib `flag` + subcommand dispatch | cobra only if the verb surface grows. |
| D6 | **Go version** | `go 1.24` floor, local 1.25 toolchain | `x/tools` + OTel SDK already in module cache. |
| D7 | **Config** | `.flowmap.yaml` via `gopkg.in/yaml.v3` | Per service; classify hints + tier rules; everything else defaulted. |
| D8 | **Trace capture** | Real OTel SDK + `sdk/trace/tracetest` recorder | Adapt to an internal span model so `canon`/`ir`/`diff` never import OTel. |

### Corrections folded in from the pressure test (deltas vs. the first draft)
- **[C1] Public vs. internal split.** `harness`, `flow`, `ir` are **exported, module-root packages** (service repos import them to author flow tests — `internal/` would forbid that). The analysis engine stays under `internal/`.
- **[C2] Concurrency is structural, not timing.** Sibling order is sequential only on a *forced* happens-before; otherwise concurrent (spec rule 3). The determinism self-test runs **capture** 2–3× (varying scheduling), not just canon on one capture.
- **[C3] Salience filtering = tree contraction.** Dropping a sub-threshold internal node must **promote** its surviving children to its parent's slot, preserving order/concurrency. Acceptance is generated from flowmap's *own* fixture, not byte-pinned to the hand-built example (spec §7 shows a pre-filter tree; artifacts §4 shows the post-`warn` tree — they legitimately differ).
- **[C4] Naming + scope reconciled** (product `flowmap`; full scope; module path as confirmed).
- **[C5] Per-service analysis unit.** The unit is **one service dir** in a monorepo (`services/<svc>/`), bus + sibling services = boundary — not "the whole repo."
- **[H1] SQL normalization is tokenizer-grade** (`db.statement` is the most volatile gated value).
- **[H2] Coverage join is specified** (behavioral `PUBLISH x` ↔ boundary event `x`, etc.).
- **[H3] The `flow`/`harness` test DSL is a first-class component**; cardinality is enforced by the **test runner**, not the diff.
- **[H4] Multi-module workspace** (`go.work`): fixtures/real repo are separate modules loaded via `go/packages`.
- **[H5] New blind-spot category** `NonConstantBoundaryArg` (publish/RPC with a non-literal target) — the artifacts' only blind-spot example.
- **[H6] Artifact schema versioning** (`schema_version`) + a regeneration/migration story; `Discards` excluded from golden equality (no volatile counts in gated bytes).
- **YAGNI trims:** drop renderer `alt`-for-branches (IR has no branch; error flows are separate goldens); defer the AST-position detail sidecar to the non-gated/AI work.

---

## Architecture: module & package layout

Module `github.com/jyang234/golang-code-graph`; binary `flowmap`; Go 1.24 (built with local 1.25). A **`go.work`** ties the engine module to the fixture module(s) and (optionally) the real lighthouse repo.

```
go.work                          # engine + testdata/fixtures/loansvc (+ optional real repo)
cmd/flowmap/                     # CLI: boundary [--check] | graph --entry | coverage | diff | version

# ---- PUBLIC (semver-stable; imported by target service repos) ----
harness/                         # NewInProcess(t): wires real router + in-mem exporter + optional testcontainers DB; (Phase 10) NewPostHoc
flow/                            # test DSL: New().Trigger().WithBody().ExpectExactlyOnce().Tier().Run(t, app)
ir/                              # authoritative CanonicalTrace/CanonicalSpan/ChildGroup/Kind/DiscardManifest (stable data contract)

# ---- INTERNAL (engine; not importable externally) ----
internal/
  canonjson/                     # deterministic JSON: recursive key-sort, stable, no timestamps/counts
  model/                         # shared Features struct + enums (Boundary/Effect/Origin)
  glob/                          # '*' = any run incl. separators (NOT path.Match)
  config/                        # .flowmap.yaml loader (classify hints, tier rules/pins, canon knobs, per-flow decls)
  tiermap/                       # Classify(Features)->Tier: ordered rules + pins + 9 defaults + catchAll
  static/
    loader/                      # go/packages load+typecheck for ONE service unit; first-party siblings via go.work/go.mod
    ssabuild/                    # go/ssa (InstantiateGenerics)
    callgraph/                   # RTA (default) / VTA (refine) / CHA (fallback) + algo+caveat record
    roots/                       # synthetic roots: mains ∪ handler/consumer func-values (hints) ∪ exports
    features/                    # per-edge Features -> tiermap.Classify
    signatures/                  # go/types signatures
    boundary/                    # gated inter-service boundary contract (DB excluded) + schema_version
    blindspots/                  # UnresolvedDispatch | HighFanOut | NonConstantBoundaryArg | reflect/unsafe/linkname/cgo
    graphio/                     # non-gated full call graph + signatures (+ opt-in position sidecar, deferred)
  capture/                       # in-process capture engine used by harness; baggage->attr; concurrency signal
  await/                         # quiescence: expected-exit markers + quiet period + hard timeout
  canon/                         # CapturedFlow -> ir.CanonicalTrace (8 passes); opkey/, sql/, url/, promote/
  render/                        # ir -> Mermaid (par for concurrent; NO alt)
  diff/                          # structural diff: keyed match + LIS; taxonomy; tier prioritization
  golden/                        # load/compare/-update; used by flow.Run
  coverage/                      # static boundary − union(behavioral snapshots), joined on event/entry keys

testdata/
  fixtures/loansvc/              # toy instrumented service — SEPARATE module (own go.mod): handlers, bus pub+consume,
                                 #   pgx DB, iface w/ 2 impls, generic fn, errgroup-concurrent pair, fire-and-forget
                                 #   goroutine, a NON-CONSTANT publish (blind-spot), a decline branch (coverage gap)
  flows/ static/ config/         # committed goldens / boundary artifacts / sample configs for the engine's own tests
```

**Why `harness`/`flow`/`ir` are public:** a target repo's `flows/loan_application_test.go` does `app := harness.NewInProcess(t); flow.New(...).Run(t, app)` (artifacts §3). Module-root public packages may import the engine's `internal/*`; external repos import only these three. This is the load-bearing structural fix.

**Dependency DAG:** `canonjson, model, glob, ir` (leaves) → `config` → `tiermap`; static track `loader→ssabuild→callgraph→{roots,features,signatures}→{boundary,blindspots,graphio}`; behavioral `capture→await`, `canon`(←ir,model,tiermap,config), `render`(←ir), `diff`(←ir,tiermap), `golden`(←ir,canon,diff); `harness`(←capture,await), `flow`(←harness,canon,golden,render); `coverage`(←boundary,ir); `cmd/flowmap`←all. Static and behavioral are independent above the shared base.

---

## Phases

Each phase ends green on `go build ./... && go vet ./... && golangci-lint run && go test ./...`, plus a **run-twice-byte-identical** check for any artifact producer.

### Phase 0 — Workspace, determinism foundation, public IR
**Goal:** building, linted, CI-wired multi-module workspace whose serialization is provably deterministic.
- **Components:** engine `go.mod` + `go.work`; placeholder `testdata/fixtures/loansvc` module; `Makefile`/`.golangci.yml` (skips fixture module dirs)/GitHub Actions/`git init`. `internal/canonjson`, `internal/glob`, `internal/model`; **public `ir`**; `cmd/flowmap version`.
- **Key:** `canonjson.Marshal` (recursive sorted keys, no timestamps/counts, `\n`-terminated); `ir.*` per the authoritative def; `glob.Match` (`*` crosses `/` and `#`).
- **Verification:** marshal a map under 100 shuffled insertion orders → byte-identical; `Marshal→Unmarshal→Marshal` stable; `glob` table (`*ledger#Post` matches `internal/ledger#Post`); `go vet`/`gofmt` clean; CI green; `go.work` builds both modules.
- **Exit:** determinism primitives proven; workspace compiles; `ir` frozen.

### Phase 1 — Shared tier-map classifier + config
**Goal:** the pure classifier used 3× (static edges, canon salience, diff priority).
- **Components:** `internal/config` (tier layer), `internal/tiermap`.
- **Key:** `Config{Classify, UseDefaults, CatchAll, Rules[], Pins[]}` (**ordered slices, never maps**); `Classify(model.Features) (int, matchedRuleID)` — annotation→pins→user rules→9 built-ins→catchAll, first-match; `BuiltinRules()` in exact spec §4 order.
- **Verification:** one table case per default rule; telemetry-ranked-first (a first-party logger → 4, not 3); pin escalate/demote; shadowing (broad-before-narrow); glob pins; classify ×1000 identical.
- **Exit:** zero-config reproduces §4; precedence honored; pure & deterministic.

### Phase 2 — Static A: per-service load → SSA → roots → callgraph (+ build the fixture)
**Goal:** deterministic call graph for **one service unit**; introduce the toy fixture module.
- **Components:** `static/{loader,ssabuild,callgraph,roots}`; flesh out `testdata/fixtures/loansvc`.
- **Key:** `loader.Load(serviceDir)` (`NeedTypes|Syntax|TypesInfo|Deps|Module`; first-party siblings via go.work); `ssabuild` (`InstantiateGenerics`); `roots.Discover` (resolve func-value args to registrar hints `router.HandleFunc`/`bus.Subscribe` → `*ssa.Function`, incl. `MakeClosure`/method values; unresolved → blind-spot); `callgraph.Build` (RTA default; VTA refine; CHA fallback; record algo+caveats).
- **Verification:** roots include the handler + consumer func-values + `main`; reachability covers generic instantiation and the goroutine pair; RTA resolves the 2-impl interface to 2 callees; build twice → identical node/edge sets (sorted by FQN).
- **Exit:** per-service graph builds from discovered roots; generics/goroutine/interface verified.

### Phase 3 — Static B: features → boundary contract + blind-spots (GATED) + currency gate
**Goal:** the gated static artifact at `services/<svc>/.flowmap/boundary-contract.json` and its drift gate.
- **Components:** `static/{features,signatures,boundary,blindspots,graphio}`; `cmd/flowmap boundary [--check]`, `flowmap graph --entry`.
- **Key:** `features.Extract` (§5 table) → `Classify`; `boundary.Contract{EntryPoints,Published,Consumed,ExternalDeps, SchemaVersion}` (**DB excluded**; published from **string-constant** publish args; events are bare names like `loan.approved`); `blindspots.Manifest` incl. **`NonConstantBoundaryArg`** (publish/RPC with a non-literal target — the gated, reviewable hole); `graphio` non-gated full graph + signatures (DB edges *included* here).
- **Verification:** fixture contract = artifacts §1 shape (publishes `loan.approved/declined/disbursement.initiated`, consumes `payment.settled`, deps `credit-bureau`/`payment-gw`, two entry points), **DB absent**; the non-constant publish lands in blind-spots and changes the gated bytes; signatures incl. generics+receiver; `boundary` twice → byte-identical; `boundary --check` exits 0 clean, non-0 after adding a publish.
- **Exit:** gated contract deterministic, DB-free, schema-versioned; currency gate catches a boundary change, ignores internal refactors.

### Phase 4 — Behavioral A: PUBLIC harness + in-process capture + quiescence
**Goal:** capture a complete, scoped flow through the **real router**; fail loudly on incompleteness; get concurrency right.
- **Components:** **public `harness`**; `internal/{capture,await}`; OTel-instrument the fixture.
- **Key:** `harness.NewInProcess(t)` (real mux+middleware, in-mem exporter, `AlwaysSample`, optional testcontainers pgx; baggage→attr processor for `test.run.id`); `ir`/`trace.CapturedFlow{Flow,Trigger,Mode,Spans,Root,Complete}`; `await.Await(markers, quiet, timeout)` (root-ended ∧ all-markers ∧ idle → complete; deadline → `Complete=false`). **Concurrency signal** captured here: prefer a structural dispatch marker; fall back to caller-clock interval overlap with a guard band; **default to concurrent on ambiguity** (spec §3.3 rule 3).
- **Verification:** drive `POST /loan-application` through the real router → server Root, DB/client/publish spans, `Complete=true`; fire-and-forget goroutine span captured after quiet drain; missing marker/timeout → `Complete=false` (no snapshot); a **fast-vs-slow timing pair** (sleep one errgroup leg) yields the **same** concurrent grouping.
- **Exit:** in-process capture yields a complete scoped flow for HTTP and event triggers; truncation fails loudly; concurrency classification is timing-stable.

### Phase 5 — Behavioral B: canonicalization (the load-bearing transform)
**Goal:** the deterministic, order-insensitive `CapturedFlow → ir.CanonicalTrace`.
- **Components:** `internal/canon` (8 passes) + `canon/{opkey,sql,url,promote}`; determinism self-test.
- **Key:** `Canonicalize(cf, cfg)` refuses `!Complete`. Passes: assembly (orphans→synthetic root+flag) → id/temporal elim + redaction (replace, don't drop) → **ordering** (causal happens-before; siblings via caller-clock per Phase-4 signal) → attr allowlist + **URL template** + **tokenizer SQL normalize** (`$1`/`?`/inlined → `?`; collapse IN-lists/multi-row; `Op` keyed on `db.operation`+table) → opkey (+ derive Peer/Service) → tier (`Classify`) → **structural normalization**: loop-collapse→`Multiplicity`, retry/error-class, and **salience filtering as tree contraction** (drop tier>threshold internal nodes, **promote** their child-groups into the parent's slot preserving order+concurrency). `Discards` carries deterministic markers only (no counts). Self-test: run **capture 2–3× and canon each** → byte-identical, before any golden compare.
- **Verification:** generate from the fixture → IR matches artifacts §4 *shape* (concurrent SELECT∥score directly under root after the tier-3 evaluator is dropped+promoted; sequential charge→publishes→ledger→auditLog); parameterization (`/score/8412`→`/score/{id}`, literal SQL→`WHERE id = ?`); promotion test (tier-3 wrapper around a concurrent tier-1/2 pair → pair promoted as a concurrent group); cross-value/timing/scheduling runs → identical; `!Complete` → error. Spec §7 and artifacts §4 used as illustrations, not byte-pinned targets.
- **Exit:** transform reproduces the example's structure, is provably deterministic under timing/scheduling/value variation, contracts the tree correctly, refuses incomplete traces.

### Phase 6 — Behavioral C: renderer + golden lifecycle + PUBLIC flow DSL + cardinality
**Goal:** the committed view and the consumer-facing snapshot-assertion gate inside `go test`.
- **Components:** `internal/{render,golden}`; **public `flow`**.
- **Key:** `render.Mermaid(ir)` (`sequenceDiagram`, `par/and` for concurrent, multiplicity note; **no `alt`**; deterministic participant order). `golden` load/compare/`-update` (rewrites `*.golden.json` + re-renders `*.flow.md`; **equality ignores `Discards`**). `flow.New(name).Trigger(...).WithBody(...).ExpectExactlyOnce(op).Tier(warn).Run(t, app)` — `Run` does capture→canon→self-test→compare→**cardinality** (prescriptive vs observed `Multiplicity`, enforced by the **test runner**, independent of the diff). Expected-exit marker strings reuse `canon/opkey` so `"PUBLISH loan.approved"`/`"DB postgres INSERT ledger"` match canonical Ops exactly.
- **Verification:** render fixture IR → matches artifacts §5 (only `par`, no `alt`); golden round-trip; behavior change → test fails with a structural diff; `-update` rebases both files; cardinality violation fails even when the golden matches; the fixture's flow test runs under plain `go test` and is stable across `-count=3`.
- **Exit:** deterministic Mermaid; the public `flow`/`harness` gate works end-to-end in `go test`, `-update` included, marker grammar coupled to canon op-keys.

### Phase 7 — Structural diff + prioritization
**Goal:** IR delta → precise, prioritized change set.
- **Components:** `internal/diff`; `cmd/flowmap diff a.json b.json`.
- **Key:** keyed top-down match by `Op` (same-`Op` siblings by order); compare `{Status,ErrorType,Tier,Peer,Kind,Attrs}`→Changed; group tags→Concurrency/Cardinality; **LIS** for Reordered; unmatched→Added/Removed; prioritize **contract > tier-1 > structural > lower**; lines `[CONTRACT] +…`, `[T1] … ok→error`, `[REORDER] …`. `Attrs` compared but ranked low.
- **Verification:** one golden pair per taxonomy member; the "add fraud screening" PR (artifacts §6) → `[CONTRACT] ADDED GET fraud-svc /check/{id}` + `[REORDER]`, contract before reorder; an N-sibling reorder → minimal moved set via LIS (not delete+add cascade).
- **Exit:** every taxonomy + prioritization verified; semantic (IR-based), renderer-drift-immune.

### Phase 8 — Coverage-delta + CLI/CI wiring + real-repo smoke
**Goal:** the emergent capability; both gates wired; real-repo validation.
- **Components:** `internal/coverage`; finish `cmd/flowmap`; two CI jobs; `CODEOWNERS`; `README`.
- **Key:** `coverage.Delta(boundary, []ir.CanonicalTrace)` — **specified join**: behavioral `PUBLISH x`→event `x`, consumer-root→consumed event, client `HTTP m peer route`→external dep `(peer,m,route)`; static boundary set − union(behavioral keys) = unexercised effects. CLI: `flowmap boundary|graph|coverage|diff|version`; behavioral gate rides `go test`.
- **Verification:** fixture → coverage names `PUBLISH loan.declined` (decline branch untested) and `consume payment.settled` (no event flow), not `loan.approved` (artifacts §7 exactly); add the decline flow → it clears. **Two CI jobs:** (1) `flowmap boundary --check` = regenerate + `git diff --exit-code`; (2) `go test ./...`. Inject drift → each fails; regenerate → passes. **Real repo:** `flowmap boundary`/`graph` run clean and deterministic on the user-provided repo; blind-spots disclosed.
- **Exit:** coverage join is correct; both gates catch drift and pass clean; real-repo smoke green; docs cover author-regeneration + CI backstop + **schema-version regeneration** when flowmap's canonical form changes.

---

### Deferred surface (Phases 9–13) — only Phase 10 shipped

**Phase 9 — Error-path flows + fault injection.** A fault seam (boundary-mock/test-seam returning errors; harness §6) + per-flow error declarations. *Verify:* a downstream error yields a **separate** golden (`status: error`, normalized `ErrorType`); diff shows `[T1] … ok→error`; coverage now counts the decline/error-branch publish as exercised.

**Phase 10 — Post-hoc / out-of-process capture.** **Shipped** — in the
observe-don't-gate shape of [`post-hoc-behavioral-ingestion.md`](post-hoc-behavioral-ingestion.md),
which supersedes the sketch here. Original sketch: `harness.NewPostHoc` + `Mode=post-hoc`: OTel Collector (`AlwaysSample`) → pluggable trace-store adapter (Jaeger/Tempo/OTLP) → fetch-by-trace-id + filter `test.run.id`; reuse `await`; canon unchanged. *Verify:* fixture as a separate process → fetched IR equals the in-process IR's structure for the same flow; truncation still refused.

**Phase 11 — Inter-service E2E (cross-clock-domain).** A second toy service the fixture calls; multi-service assembly; the caller-clock rule under genuinely separate, **skewed** clocks. *Verify:* a two-service flow with injected clock skew → ordering derives only from caller client-span intervals; IR stable across skew; distinct golden from the single-service snapshot (don't conflate artifacts).

**Phase 12 — Consumer-driven contract tests (Pact-style).** Derive consumer expectations from one service's consumed-event boundary + behavioral snapshots; verify against another's published-event boundary (machine-joinable event names). *Verify:* a producer/consumer fixture pair; dropping a consumed event fails the contract; matching passes.

**Phase 13 — Queryable interface + cross-repo composition.** Query layer over the IR (upstream/downstream, per-entry-point subgraphs, boundary effects, coverage gaps); compose boundaries across repos by matching published↔consumed event names. *Verify:* queries on the fixture + real repo return expected callers/callees/subgraphs; composing two repos' boundaries reconstructs the choreography.

---

## Tricky algorithm notes

- **Root discovery (static §2):** scan SSA for `Call` sites matching registrar hints; resolve the func-value operand (`*ssa.Function` | `MakeClosure.Fn` | method value) to the synthetic root, tagged with the route/topic string-constant; unresolved → blind-spot.
- **Per-edge features (static §5):** Boundary from caller/callee pkg + I/O-hint membership + handler/consumer-root origin; Effect from hints + method shape (`Exec`→mutate, `Query`→read, `Publish`→outbound-async); Origin from import path; Fallible from signature; Concurrent from `*ssa.Go`/`defer`; Identity = FQN.
- **Concurrency, deterministically (canon §3.3) [C2]:** never cross-service timestamps. Sequential **only** when happens-before is forced (causal data dep, or caller-clock `A.end ≤ B.start` beyond a guard band); else **concurrent**, members sorted by `Op`. The static graph's `concurrent:true` (from SSA) is the authoritative cross-check for diagnostics. Self-test varies **capture**, not just canon.
- **Salience as tree contraction (canon §3.7) [C3]:** filtering deletes a node and splices its `ChildGroup`s into the parent at the node's position, preserving inter-group order and the node's own concurrency membership. Root (tier-1 entry) is never dropped.
- **SQL normalization (canon §3.4, §8.3) [H1]:** tokenizer-based — strip/parameterize literals, collapse IN-lists and multi-row inserts, normalize whitespace, unify driver placeholders (`$1`/`?`). `Op` keys on `db.operation`+table so identity barely depends on the statement.
- **Coverage join [H2]:** define `boundaryKey` over both vocabularies so `PUBLISH loan.approved` (behavioral) and `loan.approved` (boundary), and `HTTP GET credit-bureau /score/{id}` ↔ external dep `(credit-bureau, GET, /score/{id})`, compare in one key space.
- **LIS reorder (golden-diff §3):** after keyed match, items outside the longest-increasing-subsequence of matched positions are the minimal `Reordered` set.
- **Canonical JSON [det]:** sort all map keys + order-insensitive arrays; `Children` order is **semantic** (fixed upstream, never re-sorted); no timestamps/counts in gated bytes; `Discards` excluded from equality.

---

## Cross-cutting concerns

- **Public API stability [C1]:** `harness`/`flow`/`ir` are semver-stable; their exported types are the consumer contract. Changing them is a breaking change for every adopting service.
- **Artifact schema versioning [H6]:** gated artifacts carry `schema_version`; a flowmap change to canonical form bumps it and requires coordinated regeneration (`flowmap boundary` / `go test -update`) — the real blast radius; documented, not silent.
- **Determinism discipline:** one `canonjson`; sort everywhere; self-test varies capture; run-twice tests on every producer.
- **Fail loudly:** `Complete=false` refused by canon; loader errors abort; orphan spans → synthetic root + flag.
- **Multi-module workspace [H4]:** `go.work` for dev; fixtures/real repo are separate modules loaded via `go/packages`; engine `go test ./...` and golangci-lint exclude fixture module dirs.
- **Fixture strategy [D4]:** hermetic toy `loansvc` (own module, every analyzed construct incl. the non-constant publish and decline branch) for golden tests; a real instrumented repo as a non-blocking smoke target (CI artifact, not a gated golden). Testcontainers Postgres makes DB ops trustworthy but needs Docker in CI (sqlite fallback for fast local runs).
- **CI / gates / CODEOWNERS:** two jobs (currency + snapshot); CODEOWNERS routes `**/.flowmap/boundary-contract.json`, `**/testdata/flows/*.golden.json`, `**/testdata/flows/*.flow.md`, `**/.flowmap.yaml`, **and the per-flow test files** (a test change legitimately moves the baseline). Human is the oracle.

---

## Standing limitations (not buildable phases)

Per scope §7: **snapshot fatigue** (rubber-stamped golden updates) is mitigated by the prioritized diff + tier-filtered snapshot but remains a review-culture control; the system shows *change* and *coverage gaps*, not *correctness*; behavioral fidelity is bounded by the test doubles (a real Postgres is what makes DB ops trustworthy).

---

## End-to-end verification (acceptance)

1. `make verify` (`go build/vet`, `golangci-lint`, `go test ./...`) green across the workspace.
2. `flowmap boundary`/`graph` on the toy fixture run twice → byte-identical; `boundary --check` clean.
3. Behavioral golden generated from the fixture matches the artifacts §4 **structure**; self-test passes under timing/scheduling/value variation.
4. `flowmap diff` reproduces the artifacts §6 fraud-screening change set with correct prioritization.
5. `flowmap coverage` reproduces artifacts §7 (flags `loan.declined` + `payment.settled`); clears when those flows are added.
6. Both gates fail on injected drift, pass after regeneration.
7. Static extractor runs clean and deterministic on the real lighthouse repo; blind-spots disclosed.
8. A target-style flow test using only the **public** `harness`/`flow` packages compiles and gates (proves the public surface).
