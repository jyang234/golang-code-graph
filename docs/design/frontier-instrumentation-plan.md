# Frontier instrumentation — measuring and reclaiming the static frontier

> **`PROPOSAL`** · exploratory, building the case · _drafted 2026-06-15_

**Status:** Phase 1 (the classifier + attribution check) is **shipped** as
`flowmap frontier` (`internal/static/frontier`, `frontier_test.go`); the committed
fixture `testdata/fixtures/strictsvc` is its motivating measurement. Phases 2–4
(disclosure, reclaimers, the trace lane) remain designed-not-built. The framing
question this doc answers for the owner: **how do we instrument the frontier
without violating "determinism over everything"?**

First numbers from the shipped classifier (`--algo vta`):

| service | entrypoints | attribution loss | markers | B share |
|---|---|---|---|---|
| `strictsvc` (strict-server) | 3 | **100%** (3/3) | 8 | **75%** |
| `oapisvc` (non-strict, control) | 2 | 0% | 0 | — |
| `loansvc` (direct-call) | 3 | 0% | 3 | 67% |
| dogfood (`.`, this repo) | 3 | 0% | 0 (vta) / 5 C (rta) | — |

The strict-server frontier is B-dominated (reclaimable structure), every route is
severed from its effects, and the non-strict control is correctly clean — the
measurement the rest of this plan acts on. (The dogfood rta-vs-vta delta is the
algo-provenance footgun of §7, visible in the same tool.)

The toolset's job is *deterministic instrumentation for AI agents and human
reviewers* — same code in, same verdict out, no guessing. The "frontier" is every
place the static graph stops being able to answer (an unresolved dispatch, an
opaque effect). This doc is about making the frontier a **first-class, measured,
disclosed object** rather than an implicit hole — and about reclaiming the part of
it that was never actually dynamic.

---

## 0. Thesis

Three findings from the measurement drive the whole design:

1. **The frontier is mostly not dynamic.** On the one topology that stresses it
   (oapi strict-server, the production-common shape no fixture had), ~80% of the
   frontier is **reclaimable static structure** — the call-graph builder not
   crossing an `http.Handler` dispatch into a per-handler closure — not runtime
   dynamism. The lever is better static analysis, not traces.
2. **The dangerous frontier is silent.** On `strictsvc` the pipeline discloses
   **zero** blind spots while **every** boundary effect (including a classified
   `db DELETE`) is severed from the HTTP route that owns it. "What does `POST /x`
   touch?" returns nothing, with no signal that the answer is wrong. This is a
   silent-green of *structure*, distinct from the verdict silent-greens the
   R-series closed — and it hurts the agent audience most (a human can dig; an
   agent reads `blind_spots`, sees 0, and trusts it).
3. **Determinism reorders the levers.** Reclaiming (sound static analysis) and
   disclosing (describing the frontier) are deterministic; observing (traces) is
   not. So traces are the *last* resort, never in the verdict — the opposite of
   where the "opaque write surface" anxiety first points.

The design is therefore three components behind a stable taxonomy: a **classifier**
(measure), **structured disclosure** (surface), and **pluggable reclaimers**
(shrink) — with traces fenced into a separate, clearly-labeled lane.

---

## 1. The doctrine check, first (because it constrains everything)

Before any component, fix what is in and out of bounds. Every operation on the
frontier is one of four kinds; only the first two touch the deterministic verdict:

| Operation | Determinism | Touches verdict? | Verdict |
|---|---|---|---|
| **Reclaim** — add a statically-true edge the builder missed | deterministic | yes (more complete graph) | **IN** |
| **Disclose** — describe the residual frontier | deterministic | no (read-only) | **IN** |
| **Observe** — fill a frontier from traces | **non-deterministic** | only a separate lane | **fenced** |
| **Resolve-by-guess** — narrow a dynamic set to a chosen value | deterministic but unsound | yes | **OUT** |

**The load-bearing soundness argument for Reclaim.** Reclamation only ever *adds*
edges. Adding a real edge to the graph can turn a `provenAbsent` into `reachable`
(a stricter verdict) but can **never** turn `reachable` into `provenAbsent`. So
reclamation cannot manufacture a false proof of absence — the one failure mode the
whole framework exists to prevent. Even an *imprecise* reclaimer that occasionally
adds an edge execution doesn't take is safe in the dangerous direction (it can
only over-fire, a false positive, never a false all-clear). This is why
reclamation is doctrine-compatible while resolve-by-guess is not: **the forbidden
move is narrowing a dynamic target set to a guessed singleton** (e.g. resolving a
`<dynamic>` topic to "the one we think it is"), because *that* can hide a real
target and produce a false absence. Adding edges is sound-direction; removing or
singularizing them is not.

**Hard rules the rest of the doc inherits:**
- R1. The deterministic verdict surface (`fitness`/`verify`/`review`) stays a pure
  function of `(code, graph-algo)`. No component may make it depend on a trace
  corpus, a clock, or a network.
- R2. A reclaimer ships only with a proof (or test-backed argument) that it adds
  edges real execution can take. Soundness is the admission ticket; precision is a
  bonus. Unsound-but-useful is **out**.
- R3. The classifier's A/B labels are **descriptive instrumentation**, never a
  verdict input. A mislabel must never change a `fitness` outcome — it can only
  misprioritize our own work. (Guards against the classifier's heuristics leaking
  into soundness.)
- R4. Observation (traces) lives in a physically separate artifact and may only
  **escalate** a Caution to a Violation, never relax any verdict. It is labeled
  trace-informed everywhere it surfaces.

---

## 2. The taxonomy (the design's type system)

A stable, machine-readable classification of every frontier marker. This vocabulary
is the generic backbone — components 1–3 all speak it.

- **A — truly dynamic.** Resolved only at runtime: `<dynamic>` bus topics / HTTP
  targets (`graphio/labels.go` emits these), reflection dispatch, plugin/registry
  table dispatch. *Irreducible statically.* Disclose; optionally observe.
- **B — reclaimable structure.** Statically determined but unconnected by the
  current builder: the strict-server `$1` seam (forward-starvation), constant
  values not folded, handler tables built from literals. *Reclaimable, sound.*
  This is where the static lever lives.
- **B2 — reclaimable by code change.** Opaque only because the *source* is
  non-constant: `db ExecContext` from runtime-built SQL. The consumer can make it
  constant and get a real proof; we can't reclaim it for them, but we can disclose
  the ask precisely. *Consumer-reclaimable.*
- **C — over-approximation.** Sound but imprecise: HighFanOut shared-middleware
  dispatch (`blindspots/blindspots.go`; see `wrapper-fanout-investigation.md`).
  The width is real; VTA narrows it but it stays wide. *Not blindness — precision.*

Each frontier marker the pipeline produces gets exactly one bin. The classifier
assigns it; disclosure reports it; reclaimers register against B; the consumer
acts on B2; C is a known-and-accepted posture.

---

## 3. Component 1 — the frontier classifier (productize the throwaway) — SHIPPED

**What:** given any analyzer-produced graph, emit a deterministic inventory of its
frontier, binned A/B/B2/C, with per-marker location and (for B) a reclaimer hint.

**Where (built):** `internal/static/frontier` (a producer-side package so Phase 2
can reuse it to emit the disclosed section), exposed as `flowmap frontier [--algo]
[--json] <dir>` — human summary by default, canonical JSON with `--json`, same
dual-output discipline as `boundary`/`graph`. It is a measurement: it never fails
closed and imports no verdict surface.

**Two precision rules that survived contact with real graphs (both tighten B so the
"reclaimable share" can't be inflated):**
- A `severed-closure` is B only if it **reaches a boundary effect** — a leaf
  callback (sort comparator) is also a parentless `$N` node but hides nothing, and
  a `parent → callback` edge would usually be FALSE (the parent *passes* it, not
  *calls* it). This is the soundness gate (Gate 2) applied at measurement time.
- A `starved-entrypoint` is flagged only when the route **owns a severed
  effect-bearing closure** — distinguishing a dispatch-severed route (strict-server)
  from a genuine no-op stub (which owns no effect closure, so nothing to reclaim).
  Without this, the empty `GetLoanApplicationStatus` stub in non-strict oapisvc
  false-flagged; with it, oapisvc is correctly clean.

**Inputs:** the graph (`graphio.Graph`) plus the markers already in it — boundary
effect labels, `blind_spots`, and *derived* structural markers (orphan-root
closures whose lexical parent exists; HTTP-entrypoint roots with empty cones).

**Classification rules (deterministic, documented, descriptive only — R3):**
- effect label contains `<dynamic>` → **A**
- `boundary:db <verb>` with an unreadable verb (method-name fallback in
  `dbLabel`) → **B2**
- blind spot kind ∈ {reflect, unsafe, cgo, go:linkname} → **A**; {HighFanOut} → **C**
- a `$N` closure that is a root (no caller) whose de-`$N` parent is a graph node →
  **B** (severed seam), with the parent recorded as the reclaim target
- an entrypoint root with an empty forward cone → **B** (starved route), flagged
  prominently (it breaks attribution)

**Output (per marker):** `{kind, bin, site, owner?, reclaimer_hint?}`, plus
roll-ups: counts per bin, and the two headline ratios — *reclaimable share*
(B / total) and *attribution loss* (entrypoints with empty cones / entrypoints).

**Why generic:** it speaks the taxonomy, not any one framework. It runs on
loansvc, dogfood, strictsvc, and a future real service identically. **Its job is
to keep us honest about prevalence** — we build a reclaimer (component 3) for a
shape only when the classifier shows that shape is common, not because one field
report named it. That is the answer to "are we hyper-molding": the classifier is
the anti-molding instrument.

**Coverage limit — be honest about it (do not over-read a low attribution loss).**
The *taxonomy and bins* are framework-agnostic, but one detector is not: the
`starved-entrypoint` marker fires only when a route's effect lives in a `$N`
closure lexically nested in the registered handler — the oapi-codegen strict-server
shape. A framework that severs differently (grpc-gateway / connect / gin route work
in a separate generated function or a method on another type) produces no
`starved-entrypoint` marker, so **a 0% attribution loss means "no CONFIRMED seam",
not "no severance".** This is a deliberate precision choice — the correlation is
what makes a starved marker a confirmed seam rather than a guess — but it must be
*disclosed*, not papered over with a "framework-blind" claim. Each new severance
shape earns its own confirming correlation (and a fixture), the same prevalence gate
reclaimers use.

**The three-valued completion (the trust fix).** A prose caveat is not enough,
because the audience that over-reads a clean number — the agent — reads the *data*,
not the prose. So the classifier mirrors `fitness`'s three-valued verdict
(`provenAbsent` / `noPathFound` / `reachable`): a route reaching no effect is
**confirmed-severed** (the oapi correlation → a `starved-entrypoint` marker, counted
in `attribution_loss`), **proven-clean** (reaches an effect), or **unconfirmed** (a
no-op, or a seam shape we do not recognize). The unconfirmed population is exposed
machine-readably so `attribution_loss: 0` can never be misread as a proof of no
severance — it is a LOWER BOUND, framed exactly like the `io_budget` "lower bound /
frontier blind" caution. Surfaces, split so the committed artifact stays stable:
- **Committed graph `frontier` section** → the AGGREGATE `unconfirmed_routes` count +
  a `coverage` string. A count (not per-route markers) so it does not churn under
  refactoring or cry wolf on every health endpoint, yet it makes the section
  self-describing: a consumer reading only the graph sees the lower-bound caveat.
- **On-demand `flowmap frontier`** → the per-route unconfirmed list, where verbosity
  is acceptable because the reader asked for it and it never pollutes the artifact.

**Determinism:** pure function of the graph; sorted, stable; no verdict coupling.

---

## 4. Component 2 — structured frontier disclosure — SHIPPED

**The gap it closes:** today `blind_spots` carries only the
reflect/HighFanOut/unsafe family (`graph.go` `BlindSpot`); structural starvation
(severed closures, empty-cone routes) is **implicit in topology** and invisible to
a consumer reading the disclosure. Finding 2: the agent reads 0 and trusts it.

**Built:** `graphio.Graph` gained a typed `frontier` section (omitempty — a
frontier-free service emits a byte-identical graph), populated by `graphio.Build`
over the finalized graph; `flowmap graph` now emits it. The consumer
(`groundwork/graph.Graph`) decodes it on its own side of the trust boundary
(`DisallowUnknownFields`-safe) and exposes it via `Index.Frontier()`. Kept SEPARATE
from `blind_spots` (D1): `blind_spots` is verdict-coupled (it raises Cautions),
the `frontier` section is read-only — no verdict reads it. The golden manifest
ratchet now pins per-service frontier counts, so a future analyzer change to the
section faces a reviewer.

**Original intent:** promote the derived B-markers into the graph's disclosed
frontier as a new, typed `frontier` section (D1 — kept separate from the
verdict-coupled `blind_spots`), so the frontier is something a consumer can *read*,
not *reconstruct*. An agent
asking "can I trust this route's effect list?" gets a direct, machine-readable
"no — this route's cone is severed at `<site>` (kind: strict-server-seam, bin: B)."

**Determinism & doctrine:** disclosure is read-only w.r.t. verdicts (R1). It does
**not** change a `fitness` result; it changes what the consumer is *told*. This is
the "instrument the wall" move from the field discussion — for the irreducible-A
remainder it is the *only* honest option, and even for B it is what carries the
gap until a reclaimer lands.

**Consumer surfaces:** the ground card / review already echo provenance; they gain
a frontier summary. The three-valued verdict is unchanged — a `noPathFound`
Caution can now cite the structured frontier instead of prose.

---

## 5. Component 3 — pluggable reclaimers (shrink B, soundly) — SHIPPED (opt-in)

**Built:** `internal/static/reclaim` with the first reclaimer (`StrictServer`),
folded in by `graphio.ApplyReclaimers` and exposed opt-in as `flowmap graph
--reclaim` / `flowmap frontier --reclaim`. On strictsvc it recovers the three
wrapper→`$1` edges the builder lost at the `http.Handler` dispatch, and the frontier
goes from **100% attribution loss / 6 B markers → 0% / 0 B** — every route reaches
its effects, leaving only the irreducible A/B2 frontier. Default Build is unchanged
(reclaimers are opt-in, D2), so no golden churns. Each edge carries `via:
"strict-server"`. Soundness (R2) is enforced by the detector below and the
no-false-positives test (oapisvc/loansvc recover nothing).

**Consumer side (R9):** the groundwork decoder (`groundwork/graph.Edge`) now
accepts and round-trips the per-edge `via` tag — without it `DisallowUnknownFields`
rejected every reclaimed graph (`unknown field "via"`, exit 2), so the reclaimer was
producible but not consumable through `init`/`fitness`/`verify`/`review`/`reach`.
The substrate disclosure is extended too (`Graph.ReclaimCaveat`): when a graph
carries reclaimed edges, the substrate line every verdict surface echoes appends
`reclaim-informed: N via <reclaimer> …`, so a verdict that leaned on a reclaimed
edge is auditable as such — the Algo/Caveats discipline (R3) extended to the
reclaimer provenance.

**What:** sound static-analysis passes that add the missing edges for a recognized
B-shape. First target, measured as ~80% of the strict-server frontier:

> **strict-server-seam reclaimer** — when a `ServerInterfaceWrapper` method builds
> an `http.HandlerFunc(closure)`, stores it in an `http.Handler`, and dispatches
> via `ServeHTTP`, add the edge `wrapper → closure`. Adding it is sound (R2) and
> reconnects the whole severed chain to the route.
>
> **As built**, soundness is enforced by a flow check, not the framework name: the
> edge is emitted ONLY when the closure value provably FLOWS, within the method, to
> a `ServeHTTP` receiver (traced back through MakeClosure/MakeInterface/ChangeType/
> Phi). With empty middleware `handler.ServeHTTP(w,r)` IS the closure call directly,
> so it CAN be invoked — a true can-reach edge; a closure the method merely PASSES
> elsewhere (a sort comparator, a route registration) never reaches a `ServeHTTP`
> receiver and is left alone. That flow requirement is the concrete form of Gate 2.

**Architecture — generic interface, specific plugins.** This is the explicit
answer to "generic vs hyper-focused":

```
type Reclaimer interface {
    // Match reports the edges this reclaimer can soundly add for a node, or nil.
    // Each returned edge MUST be one real execution can take (R2).
    Reclaim(res *analyze.Result, fn ssa.Function) []Edge
    Name() string          // provenance: which reclaimer added the edge
    Bin() string           // always "B"; for the inventory
}
```

- The **framework** (registry, the "edge carries its reclaimer provenance" plumbing,
  the inventory integration) is generic and shape-agnostic.
- Each **reclaimer** is necessarily shape-specific — you cannot reclaim the oapi/chi
  seam without modeling oapi/chi — but it plugs into the generic interface, is
  independently sound, and is **gated by measured prevalence**: the classifier says
  which shapes earn a reclaimer.
- Reclaimed edges carry per-edge provenance (a `via` field naming the reclaimer,
  empty for base call-graph edges — D2), so a verdict can self-certify its
  substrate (the `Algo`/`Caveats` discipline in `graph.go`, extended) and a
  reviewer can see "this reachability used the strict-server reclaimer." Reclaimers
  are **opt-in behind a flag** (D2), mirroring the `--algo` default-conservative /
  refine-opt-in precedent: the base graph is unchanged and the reclaimed graph is
  an explicit, diffable superset.

This keeps us neither hyper-molded (the core is generic — the taxonomy and bins
are framework-agnostic; only the *seam-confirming correlations* are shape-specific,
disclosed as a coverage limit per §3) nor over-general (we don't build speculative
reclaimers for shapes the measurement says are rare).

**Relationship to prior art.** `wrapper-fanout-investigation.md` shipped `--algo`
and showed VTA narrows the shared-middleware HighFanOut (C) but not below the real
width. That is a *different* facet (too-many-callees at a shared site) from
forward-starvation (no-callee into the per-handler closure). VTA does not fix
starvation — we measured `strictsvc` *under VTA* and the cones are still empty. So
the reclaimer is genuinely new work, complementary to the shipped `--algo` lever.

---

## 6. Traces — the fenced lane (deferred, for completeness)

Per the doctrine, observation never enters the deterministic verdict (R4). Two
sound, presence-only uses, both **out of scope for v1** and recorded so the design
is complete:

- **Bus `<dynamic>` resolution (A).** The corpus already captures producer/consumer
  ops (`ingest/gate.go`); resolving a `<dynamic>` publisher to *observed* topics is
  the only way to recover cross-service continuity static can't. Surfaces as a
  distinct trace-informed card in `chains`, never folded into static `Cards`.
- **Opaque-write escalation (B2).** A DB-span corpus (which the effects golden
  deliberately *excludes* today) could upgrade an opaque-write route's Caution to a
  Violation on observed mutation. Sound because presence proves reachability; it
  never relaxes.

Both require **per-edge provenance** (proven / observed / opaque) and a corpus-
coverage accounting the current addition-only gate deliberately refuses — so they
are explicitly *not* a per-graph "trace mode" flag. See the field discussion and
`post-hoc-behavioral-ingestion.md`. **v1 ships none of this**; the deterministic
core must stand alone first.

---

## 7. Other gaps to consider

Beyond the strict-server seam, the classifier should bin (and we should decide a
posture for) these. Most are unmeasured today — the classifier is how we find out
which are real here:

| Gap | Bin | Posture |
|---|---|---|
| Reflection dispatch (`reflect.Value.Call`) | A | disclose; never resolve |
| Plugin / registry tables (`map[string]Handler`) | A (mostly) | disclose; a literal-only table is B (reclaimable) |
| Config / data-driven routing | A | disclose |
| Interface DI (constructor-injected impls) | C→B | VTA narrows; a single-binding interface is reclaimable |
| Generics instantiation | B | ensure instantiations are nodes, not a frontier |
| Struct-field / map-stored callbacks | B or A | literal-assigned = B; runtime-assigned = A |
| `defer`/goroutine spawn edges | (modeled) | already `Concurrent`; verify no starvation analogue |
| cgo / unsafe / go:linkname | A | disclose (already blind spots) |
| Dynamic outbound HTTP/RPC target | A | `<dynamic>` already; same treatment as bus |
| ORM / query-builder SQL | B2 | consumer-reclaimable; disclose the ask |

**Meta-gaps (not frontier markers, but adjacent and worth folding in):**
- **Entrypoint→effect attribution as a first-class check.** Our headline finding —
  routes with empty cones — deserves its own deterministic signal ("N routes touch
  nothing; suspicious"), independent of any reclaimer. Cheap, generic, high-value
  for agents.
- **Algo-provenance mismatch (field report §9) — SHIPPED.** A policy built on VTA
  but checked on an RTA graph produced spurious violations. `init` now records the
  proposed-against algorithm in the policy's `substrate` field, and `fitness`/
  `verify` flag a policy-vs-graph mismatch (`graph.SubstrateMismatchCaveat`): a
  non-blocking Caution in fitness, a substrate caveat in the review gate, so the
  spurious findings read as an analyzer artifact, not a regression. Reused the same
  Algo/Caveats provenance plumbing.
- **Reclaimer provenance in verdicts — SHIPPED (R9).** Edges carry a `via` source;
  groundwork decodes it and `Graph.ReclaimCaveat` states on the substrate line that
  a verdict leaned on reclaimed edges — auditability for reviewers.
- **Breaking-contract over-fire on internal churn (field report §9) — SHIPPED.**
  `review`/`verify` keyed the entrypoint contract surface on graph *roots*
  (`Sources()`), so an internal function left rootless by a deleted backend, or a
  non-route closure orphaned by a refactor, read as a BREAKING external-contract
  change. The surface was first keyed on the *external entrypoint* join (HTTP
  routes / consumed topics) so orphan roots — absent from the join — no longer
  block. This closed the orphan-root half; the closure-as-route-handler half was
  still open (R10).
- **Breaking-contract over-fire on handler-symbol movement (field report R10) —
  SHIPPED.** Keying the entrypoint join by *handler FQN* still over-fired whenever
  a route's handler symbol moved but the route was unchanged: an inline-closure
  handler (GET /livez as an anonymous func in `run()`) renumbered by an
  extract-function refactor (`run$4 → newHTTPServer$1`), or any named handler
  plainly renamed, read as a removed+added route. The delta is now keyed on the
  *route name* (`ep.Name`: HTTP method+path / consumed topic), the actual
  inter-service contract — `externalRoutes` replaces `externalEntrypoints`. This
  subsumes the §9 orphan-root exclusion (orphan roots carry no route name) and
  closes R10 in one change, with genuine route-removal detection intact.
  `TestContractNonBreakingUnderHandlerRefactorOverGeneratedGraphs` locks it across
  a generated corpus (renumbered closures, renamed named handlers, §9 churn), the
  review/contract analogue of the proposer self-clean property — the topology
  match a hand fixture had missed (R3/R7/R10).
- **Breaking-contract over-fire on effect-emitter movement (R10 sibling) —
  SHIPPED.** The same FQN-keying defect lived in the contract's effect surface:
  it keyed per boundary edge (on the emitting function, the edge's `From`), so a
  refactor that moved an emitter while the target stood — a renamed/extracted
  publisher, or a consolidation pointing several callers at one helper (in
  obligsvc, `loan.approved` is published by seven functions) — read as a
  removed+added effect, a spurious breaking change. The delta now keys on the
  effect TARGET SET (each published/consumed topic, outbound endpoint:
  `contractEffects`), so a removal fires only when the target leaves the branch
  entirely — the genuine break. It is independent of the effect ratchet
  (`newWriteTargets` already keys on the write-label set), so the write ratchet is
  unaffected. Locked by `TestContractEffectKeyedOnTargetNotEmitter` (rename,
  extract-function, consolidation, genuine removal/addition controls).
- **Universal self-clean invariant across inputs — SHIPPED.** R5–R8 were each a
  proposer/enforcer node-set gap a fixture missed.
  `TestProposeSelfCleanOverGeneratedGraphs` makes the invariant literal over a
  generated graph corpus (random topologies, strict-server seams, the full effect
  vocabulary), catching the next sibling before a field report does.

---

## 8. Determinism risk register

Explicit list of where this work could slip the doctrine, and the guardrail:

| Risk | Guardrail |
|---|---|
| A reclaimer adds an edge execution can't take, and it's load-bearing for a *bypass* finding | Edges only ever make `must_not_reach` over-fire / `must_pass_through` see more bypasses — safe direction (R2). Still: every reclaimer is test-backed against a real fixture. |
| The classifier's A/B heuristic leaks into a verdict | R3: classifier output is never a `fitness` input; a mislabel can only misprioritize our work. Enforced by keeping the classifier in a package the verdict surfaces don't import. |
| Trace observation quietly relaxes a verdict | R4: separate artifact, escalate-only, labeled trace-informed; the deterministic verdict never reads it. |
| "Resolve" creeps in (singularizing a `<dynamic>`) | Forbidden by R2's framing — narrowing a dynamic set is the one move that can hide a target. Reclaimers *add*; they never *singularize*. Reviewed per-PR. |
| Reclaimed graph becomes non-reproducible | Reclaimers are pure functions of the SSA; output stays byte-identical (the `--stamp`/regen discipline). No clock, no corpus. |
| We over-build generic machinery for one case | The classifier gates reclaimer work by measured prevalence; no speculative reclaimers. |

---

## 9. Phasing (measurement-driven)

1. **Classifier + attribution check (component 1 + the meta-gap). ✅ DONE.**
   Generic, no verdict coupling, immediate value: turns the throwaway into a
   standing, deterministic number across all fixtures. Shipped as `flowmap
   frontier` with the strictsvc inventory pinned in `frontier_test.go`. (The
   attribution-loss ratio is the meta-gap made first-class.)
2. **Structured disclosure (component 2). ✅ DONE.** The graph carries a typed,
   read-only `frontier` section (`graphio.Build` emits it, `Index.Frontier()` reads
   it, the manifest ratchet pins it) so agents read the B-frontier instead of a
   false 0. Verdict-neutral.
3. **First reclaimer (component 3): strict-server seam. ✅ DONE (opt-in).** Shipped
   sound + provenance-tagged behind `--reclaim`; closes the strictsvc seam (100%→0%
   attribution loss). Opt-in is exactly what lets it ship before the D4 prevalence
   bar is met on a real corpus: it changes no default graph, and a reviewer can diff
   base-vs-reclaimed. **Promoting it to default-on remains gated on D4** — confirm
   the shape's prevalence on a real (non-fixture) strict-server service first.
4. **(Deferred) trace lane.** Only once 1–3 stand, and only as the fenced,
   escalate-only artifact of §6.

---

## 10. Decisions (resolved 2026-06-15)

D1, D2, and D3 are locked below; they share one implementation
(provenance/disclosure metadata on graph objects) and follow the repo's existing
conventions. D4 is resolved to its qualitative form; the quantitative bar is the
one standing TODO, deferred until a representative corpus exists.

- **D1 — Disclosure shape: a new typed `frontier` section, NOT an overload of
  `blind_spots`.** Two reasons: (a) `blind_spots` is already verdict-coupled —
  `frontierBlindSiteWith` turns any in-cone blind spot into a `noPathFound`
  Caution, so adding structural-starvation markers there would let component-2
  disclosure change verdicts, violating R1; a separate section is verdict-neutral
  by construction. (b) Semantics: `blind_spots` connotes *irreducible* gaps (A/C);
  a B-marker is reclaimable and *transient* — it disappears when its reclaimer
  lands — so it must not inflate the count an agent reads as fundamental blindness.
  `blind_spots` is unchanged; `frontier` carries the full taxonomy + `bin`/`owner`/
  `reclaimer_hint`. *Fork if the owner wants starvation to ABSTAIN the verdict
  (fail-safe over disclose-only): then it belongs in `blind_spots` after all. We
  chose disclose-only because the `$N` verdict gap is already covered by
  groundwork's R7 name-expansion; the durable verdict fix is the reclaimer, not a
  Caution flood.*
- **D2 — Reclaimer trust: per-edge provenance (a `via` field on `Edge`),
  reclaimers OPT-IN behind a flag.** Per-edge provenance subsumes "visibly
  distinct" — distinctness is a filter or a base-vs-reclaimed diff away — and it is
  what lets a proof self-certify which reclaimers it used (the §7 auditability
  meta-gap). Opt-in mirrors the shipped `--algo` precedent (conservative default,
  refinement opt-in): the base graph stays unchanged, the reclaimed graph is an
  explicit diffable superset. Promote to default-on only after a real-corpus
  soundness bake. *Fork: a proven-sound reclaimer could justify default-on sooner
  than `--algo` did — a trust-vs-friction call for the owner.*
- **D3 — B2 ergonomics: actionable per-site disclosure, NO codemod.** Emit the
  exact call site + reason (arg 0 non-constant → verb unreadable) + the concrete
  ask ("hoist the SQL to a `const`"). For an agent that site+ask IS the work item —
  B2 is just component 2 applied to opaque writes, and reuses D1's `reclaimer_hint`
  plumbing. A codemod is over-reach: rewriting query construction is risky and
  framework-specific, stepping the analyzer into refactoring.
- **D4 — Prevalence bar: two qualitative gates now; the percentage deferred.** A
  shape earns a reclaimer only if it passes BOTH: (Gate 1, breadth) it comes from a
  *named framework/codegen pattern* recurring across a CLASS of services, not one
  bespoke codebase — kills the long-tail-of-one-offs risk; (Gate 2, soundness) the
  reclaim is a *local, statically-provable edge-add* (closure lexically present,
  unconditionally invoked), reviewable in isolation, not a cross-procedural
  heuristic. Strict-server passes both; reflection and registry-table dispatch pass
  Gate 1 but fail Gate 2 → they stay disclosed, never reclaimed — the line falls
  out of the doctrine, not a magic number. **Standing TODO:** a quantitative
  prevalence threshold, blocked on a representative multi-service corpus (dogfood is
  framework-free; the fleet is unseen). The classifier reports prevalence
  opportunistically (§3) so the number accrues for later tightening; v1 does not
  block on it.

---

*Companion artifacts: `testdata/fixtures/strictsvc` (the measured topology) and
`internal/static/boundary/strictserver_test.go` (the deterministic characterization
that flips when component 3 lands). Prior art: `wrapper-fanout-investigation.md`
(the HighFanOut facet + `--algo`), `post-hoc-behavioral-ingestion.md` (the trace
lane), field report §9 (algo provenance) and §10 (dynamic frontier).*
