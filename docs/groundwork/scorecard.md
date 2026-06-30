# Capability scorecard — an honest assessment

> **`DESIGN RECORD`** · capability assessment, graded by evidence (re-grade on new evidence) · _reviewed 2026-06-24_

**As of:** 2026-06-24 — adds the **reviewer-triage prototype** (`review-triage`) as a
⚠️ value-unproven row (mechanics unit-locked, field value unmeasured). The prior
re-grade (2026-06-18, branch `claude/phase-4-5-prime-directive-risk-er9gam`) added
**behavioral impeachment** (Phases 0–5: the `observed ×
proven-absent` counterexample finder, the corpus gate, and the audit-only `impeach`
MCP lens), the producer-set **capture-fidelity provenance**, and the
determinism/fail-closed hardening wave. The re-grade before that (2026-06-16, HEAD
`45d70bd`) added the static-frontier classifier, the strict-server reclaimer, and
`--expect` commit-identity gate binding.
**Purpose:** what this toolset can actually do, graded by *evidence class*, with
each capability's known limits beside its strengths. Re-grade when the
evidence changes; the drill record (`drills.md`) and the test suite are the
re-grading instruments.

## The evidence classes (three-valued honesty, applied to ourselves)

| Grade | Meaning |
|---|---|
| ✅ **Proven** | Locked by committed tests/goldens; a regression fails the suite |
| 📐 **Measured** | Quantified by committed drills against the dogfood fixture — real numbers, small-fixture caveat |
| 📋 **Designed** | Specified with named criteria and a results slot, not yet run |
| ⚠️ **Unproven** | No evidence either way; graded honestly as such |

Nothing below is graded on intention. A capability that works in demos but has
no committed lock would be ⚠️, not ✅.

---

## flowmap (the producer)

| Capability | Grade | Evidence | Known limits |
|---|---|---|---|
| Call graph + typed boundary effects | ✅ | Golden-locked on four fixtures; byte-deterministic, regen-gated | RTA over-approximates interface-dense code; blind spots disclosed, not eliminated |
| Gated boundary contract (currency gate) | ✅ | CI-proven (`boundary --check`); breaking-change diff tested | Inter-service surface only; no cross-service composition of contracts |
| Behavioral golden snapshots (in-process) | ✅ | Dogfooded end-to-end on loansvc; snapshot-assertion gated | Requires OTel instrumentation + flow-test authoring — the largest adoption ask in the toolset |
| Post-hoc OTLP ingestion | ✅ | Dogfood test proves wire-format round-trip equals in-process golden; E2 drill stages an incident through it | Tail-sampling/collector config is the adopter's problem |
| Path obligations (must-release / must-precede) | ✅ | Six review-confirmed idioms locked as unit tables AND fixture goldens; failure-branch pruning, closure credit, defer-rooted recover all reproduction-tested | **Intraprocedural and value-blind by design**: release-in-an-unlisted-helper reports VIOLATED (the rule vocabulary is the fix); dynamic deferred values (`defer cancel()`) are an accepted recover-detection residual |
| Partial-effect facts (`effect_order`) | ✅ | Disburse scenario locked (dominating publish → always; branch-arm → possibly); negative cases tested | **Same-function orderings only** — disclosed on every fault card; absence is never an all-clear |
| Entrypoints join (route/topic → fn) | ✅ | Resolver-tested incl. method-less roots, mount prefixes, param wildcards | Registration-site literals; gin/gorilla/gRPC routes absent (loud no-match); middleware resolves to the wrapping closure |
| Graph stamping (`--stamp`) | ✅ | All four verify behaviors tested; goldens proven unstamped/unchanged | Caller-supplied only — verifies the claim chain, not the deploy pipeline's existence |
| Static-frontier classifier (`flowmap frontier`) | ✅ | Classifier + attribution check locked on hand-authored graph fixtures and the `strictsvc`/`oapisvc`/`loansvc` services (`internal/static/frontier/frontier_test.go`, `frontier_classify_test.go`); the three-valued disclosure (confirmed-starved / unconfirmed / clean) is tested both ways so a 0-loss can't be misread | **Measurement, not a gate** — imports no verdict surface; attribution loss is a *lower bound*, not a proof; whole-service only (a scoped `--entry` build carries no frontier section by design) |
| Strict-server seam reclaimer (`flowmap graph --reclaim`) | ✅ | Recovers exactly the strict-server dispatch edges, each tagged with `via` provenance, and **zero false positives** on non-seam services (`internal/static/reclaim/reclaim_test.go`); folding the edges in drives the frontier's attribution loss to 0 | Opt-in by design (default graph and goldens unchanged); covers only the oapi strict-server seam shape; promotion to default-on is gated on real-service prevalence evidence (not yet collected) |
| Middleware-chain reclaimer (`flowmap graph --reclaim-middleware`) | ✅ | Resolves the `for _, mw := range HandlerMiddlewares { h = mw(h …) }` loop to its concrete funcs when the set is statically provable (const literal / append chain / empty), in BOTH the http layer (`mw(h)`, `ServeHTTP` terminal) and the oapi-codegen strict-server layer one level deeper (`mw(h, "Op")` over `[]StrictHTTPMiddlewareFunc`, func-value `h(…)` terminal); tags edges `via=middleware-chain`, and clears the `UnresolvedCall` seam only when the set is provably empty AND the threaded handler is the sole terminal; soundness guards (field-slice escape, append-result aliasing, sibling-return, anchored type-match) and all poles tested (`internal/static/reclaim/middleware_test.go`, `graphio/middleware_test.go`, `graphio/middleware_internal_test.go`, fixture `mwchainsvc`) | Opt-in by design (default graph and goldens unchanged); a dynamic/escaping middleware source stays disclosed; the non-empty chain reconnects edges but its per-middleware re-dispatch hop (`next.ServeHTTP` / strict `f(…)`) stays an honest residual seam |
| Capture-fidelity provenance (producer-set + reconciled) | ✅ | The grade is producer-set (the harness marks captures `integration`, a deploy sets `production` via a resource attribute), self-described by the committed corpus, and reconciled in `impeach.Audit`: a caller-asserted grade that contradicts the capture fails CLOSED to unestablished; only `production`/`integration` may be asserted (`capture.AssertableGrade`, one source for the verify CLI and MCP); §12.6 tests | **No cryptographic attestation yet** — a mislabeled producer is trusted (a signing authority is the named next step); `synthetic`/absent never promote, by design |

### Coverage boundary — the middleware-chain reclaimer

The `--reclaim-middleware` pass recovers the oapi-codegen/chi middleware-application
seam in BOTH layers oapi-codegen emits: the http layer
(`for _, mw := range siw.HandlerMiddlewares { h = mw(h) }; h.ServeHTTP(w, r)`) and the
strict-server layer one level deeper, where the element type is
`StrictHTTPMiddlewareFunc`, the call carries the operation id
(`for _, mw := range sh.middlewares { h = mw(h, "Op") }`), and the terminal dispatches
by CALLING the threaded handler as a func value (`h(ctx, w, r, req)`) rather than via
`ServeHTTP`. The same recurrence — `h` fed back by the call result, here through the
identity `ChangeType` conversions go/ssa inserts between the closure's func type and the
named `StrictHTTPHandlerFunc` — drives both, so the same provably-empty clearing applies.
Because a recovered edge under an *absence* proof is a false PROVEN — the worst
outcome (CLAUDE.md tenet 4) — the boundary between what it proves and what it
refuses is itself a soundness claim, stated here in full so "statically
determinable" is never oversold.

**Proves** (resolves the `mw(h)` call to concrete callees, or clears the
`UnresolvedCall` disclosure), each shape regression-locked in
`internal/static/reclaim/middleware_test.go` against fixture `mwchainsvc`:

| Middleware-set shape | Outcome | Fixture wrapper |
|---|---|---|
| Const slice literal (`[]MiddlewareFunc{a, b}`) | Edges to `a`, `b`, tagged `via=middleware-chain`; NOT cleared (non-empty) | `KnownWrapper`, `StrictKnownWrapper` (strict layer) |
| `append` chain (`append(append(nil, a), b)`) | Edges to every appended func | `AppendWrapper` |
| Param-field copy / transitive-empty bootstrap (`HandlerMiddlewares: options.Middlewares`, nil-in-prod, through *N* hops) | Cleared — the field is provably empty across the whole closed program | `strictsvc` (real two-hop `HandlerFromMux` → `HandlerWithOptions`) |
| Provably-empty loop (no store, or only empty stores) | Cleared — seam resolves to the threaded handler alone | `EmptyWrapper`, `InlineWrapper` |
| Provably-empty strict-server loop (`mw(h, "Op")`, func-value `h(…)` terminal) | Cleared — same recurrence/empty proof, one element type and one constructor deeper | `StrictEmptyWrapper` |
| Reverse-index loop (`for i := len(m)-1; i >= 0; i--`, the `ApplyChiMiddlewareFirstToLast` variant) | Same as forward — keyed on the `mw(h)` phi, not the induction form | (verified ad hoc; not fixture-locked) |

**Abstains** (site stays `UnresolvedCall`, no edge added, no disclosure cleared —
fail-closed), each pole tested:

| Middleware-set shape | Why it abstains | Fixture wrapper |
|---|---|---|
| Opaque/dynamic source (slice from a global, a parameter, an unanalyzable call) | The set is not statically provable; an over-recovered edge would be a false PROVEN | `DynWrapper` |
| Builder-returned slice (constructed behind a call this pass can't see through) | Same — the contents are not enumerable from the store walk | (subsumed by `DynWrapper`) |
| Field that escapes to a mutating write (`IndexAddr` store, range-with-write, address taken to a non-same-field sink) | The empty/known proof no longer holds once the field can be mutated out of view | `EscapeWrapper` |
| Sibling-return ambiguity (a factored terminal where some return path does *not* yield the threaded handler) | Clearing requires *every* return to thread the handler; one that doesn't breaks the proof | `SibWrapper` |

**The load-bearing soundness caveat — closed-program (service) assumption.**
Every "Proves" outcome above rests on the reclaimer seeing *every* store to
`HandlerMiddlewares`. That holds for an analyzed **service**: the program is
closed, so the `ssautil.AllFunctions` walk enumerates all bootstrap sites and a
field that is never stored-to (or only stored empty) is genuinely empty at every
call. It does **not** hold in **library mode**, where an external caller outside
the analyzed unit could populate the field after analysis. In that mode the
clear/resolve is *not* sound and the site should stay disclosed; the pass is
opt-in and service-scoped precisely so this assumption is explicit rather than
silent. Promotion to default-on, or to library targets, is gated on making that
assumption checkable — not yet done.

**Honest residual even when it proves.** For a *non-empty* chain the pass
reconnects the `mw(h)` edges, but each middleware's own re-dispatch hop remains an
`UnresolvedCall` — the http layer's `next.ServeHTTP(w, r)` or the strict layer's
`f(ctx, w, r, req)` — an honest residual seam, not a recovered one. The reclaimer
narrows the blind spot; it does not claim to eliminate it.

## groundwork (the judge)

| Capability | Grade | Evidence | Known limits |
|---|---|---|---|
| Six policy families (layering, must_not_reach, must_pass_through, no_concurrent_reach, io_budget, blind-spot ratchet) | ✅ | Each family has fixture verdicts both ways; `entrypoint:*` binds everywhere; fail-closed on unknown vocabularies | `concurrent` flag conflates go/defer (disclosed; split planned on evidence); path-insensitive over-approximation throughout |
| Review artifact + digest + verify-artifact | ✅ | Tamper, stale, and **re-signed forgery** all caught by tests; abstention suppressed by any new signal | Digest is not the anchor — recomputation from CI-generated graphs is; set-based delta misses new-call-site-to-already-called-target (documented) |
| Pre-flight gate (`verify`) | ✅ | Blocks on new violations / scope creep / breaking contract / gated blind spots; fixture-proven | Only as trustworthy as graph generation — the trust boundary is CI wiring, not this binary |
| Reviewer triage (`review-triage`) — **PROTOTYPE** | ⚠️ | *Mechanics* are unit-locked: the three-zone partition (new-blind / carried / accounted) with the new-vs-carried diff-delta, forward-only + severity-aware zoning, consequence ranking, the four renders (markdown / `--summary` / `--mermaid` / `--json`), the attention-gradient scale rollup, and per-route movement reusing `review.RouteIODeltas`. The reviewer-legible `--summary` adds a why-blind taxonomy (masking / runtime-dispatch / unresolved callee / external handoff / over-approx / routine-telemetry, fail-loud on unknown packages, per-domain masking gate) and `--scope-fqns` partitions author-edited blindness from callee-dragged-in, with a seam-level soundness rule (an authored blind callee promotes its caller) and fail-loud zero-match fallback; determinism pinned, no AI in any output | **Built, not field-validated** — a comprehension aid, never a gate and no verdict (so the `⚠️` is its *value*, not its soundness): whether the framing actually speeds a human reviewer on a real diff is unmeasured, and "accounted" is structural completeness, never approval. Novelty uses the set-based delta (inherits `changedFns`' new-call-site-to-existing-target limit); the author-edited FQN set is a caller input — and `flowmap graph` now carries the per-node `file`/`line`/`end_line` declaration span the caller intersects a `git diff` against to produce it (disclosure-only, byte-identical, no verdict reads it). Honest next step: run it on a real MR |
| Commit-identity gate binding (`--expect`) | ✅ | `--expect <sha>` binds fitness/review/verify/verify-artifact to the branch graph's stamp; a mismatch fails *operationally before* the verdict, and `GROUNDWORK_REQUIRE_STAMP` turns a forgotten flag into a CI failure rather than a silent skip (`cmd/groundwork/gatestamp_test.go`) | Stamp is caller-supplied (verifies the claim chain, not that a deploy happened); boundary contracts deliberately out of scope; inert without the flag (no golden churn) |
| Exceptions audit (dead-entry detection) | ✅ | Set-based attribution; the caution↔violation swap that fooled the count-proxy version is the regression test | Liveness is per-graph: an entry dead on this service may be live on another sharing the policy |
| Incident triage (5 symptom kinds) | 📐 | **10/10 recall, median 8% hunt space, route scenarios 3%** (E1); trace handoff proven end-to-end (E2); staleness mis-scope demonstrated (E3); thresholds are committed assertions | Fixture is 39 nodes and well-factored — fractions will grow on monoliths (why the thresholds are ratchets); non-code causes are out of scope, stated on every fault card |
| Partial-effect fault answers | ✅ | Certainly/possibly split locked; scope statement prints even when sections are empty | Inherits effect_order's same-function limit |
| Ground cards (pre-edit binding rules) | ✅ | The defining test seeds the violation the card warns about and asserts the named rules fire; same matchers as the checks | Binding ≠ exhaustive: only declared rules appear; an unconfigured hazard is invisible by definition |
| MCP server | ✅ | Scripted-session tests: handshake, discovery, cards, isError tool results, -32601; fleet session: prefixed entrypoints, fleet-events join, explicit-hop errors; HTTP session: bearer auth, Origin rejection, 405/202/400 transport discipline, fail-closed exposure guard | Staleness flagged but reload is manual by design; fleet-events covers loaded services only; HTTP auth is one static bearer token (TLS/identity belong to a reverse proxy); no SSE streams; session ids are transcript labels only, never server state; first-of-kind surface with no field hours |
| Behavioral impeachment gate (`verify --corpus`) | ✅ | Proven E2E over the `impeachsvc` fixture: a `must_not_reach` `require_proof` proof is SATISFIED statically but the committed corpus impeaches it (the missed-root DELETE), downgrading it to CANT-PROVE and BLOCKing — with causal isolation (the same policy+graph without the corpus passes; the breach is the sole block dimension); the self-extinguish loop, the witnessed-breach→VIOLATED upgrade, the `CorpusOrigin` live-vs-committed fence, and the contradicted-capture fail-closed are each locked (`internal/impeach`, `cmd/groundwork/verify_corpus_test.go`); the gate certificate now **names the severance localization** (missed-root + site, not just "a proof was downgraded"), and a corpus that fails to bind (VERSION-SKEW / CAPTURE-UNTRUSTED) is **disclosed, not silently passed** (`TestVerifyCorpusSurfacesSeveranceLocalization`, `TestVerifyCorpusNonBindingIsDisclosedNotSilent`) | **Single fixture, lab-proven not field-proven**; a *counterexample finder, not an audit* — finds unsoundness only on exercised paths, never proves static sound; **bus + DB effects only** (outbound HTTP/RPC deferred); needs a committed OTel golden corpus (the largest adoption ask); **opt-in/observe-first** (`impeachment_gate.gate` — discloses from day one, blocks only once ratified); L1 localization sound for clean-final-segment first-party code (§12.5) |
| Impeach MCP lens (audit-only) | ✅ | `disclose-the-witness-but-never-gate` (runs at `OriginLive`, so `GateBlockers` is structurally empty), byte-determinism across calls, fail-closed without `--corpus`/`--policy`, reload re-audits (IMPEACHMENT→VERSION-SKEW when the graph stamp stops matching), contradicted-capture caps below IMPEACHMENT — all locked (`cmd/groundwork/mcp_impeach_test.go`) | **Audit-only by construction** — never a gate (the loaded graph may be a local build; the gate is `verify --corpus` over CI graphs); needs `--corpus`+`--policy`; corpus is a load-once startup input (the card discloses it — restart to refresh); inherits the gate's coverage limits |
| Effectiveness drills as ratchet | 📐 | E1–E3 committed; numbers reprint on every `-v` run | They measure that triage does its job well, not that its job covers everything |
| Transcript instrument (`--log` + `transcript`) | ✅ | Byte-exact log-format test; summary semantics (id-attributed sessions surviving interleaved concurrent clients, hops through fleet-wide calls, corrections) locked by unit tests; -race concurrent-hammer test; strict decode fails closed on unknown lines | Counts measure usage, not value — E4's qualitative half (do conclusions cite card facts?) stays human-judged; no E4 field data yet |

## Cross-cutting properties

| Property | Grade | Evidence |
|---|---|---|
| Byte-determinism across machines | ✅ | Cross-checkout path-invariance test; canonical JSON everywhere; sites normalized through a total ladder; the concurrent-ordering tie-break, the canonical-JSON marshaler, SQL normalization (idempotent), and OTLP decode are each fuzz-guarded (`FuzzCanonConcurrentOrderInvariant`, `canonjson.FuzzMarshalDeterministic`, `sql.FuzzNormalizeIdempotent`, `otlpjson.Fuzz*` — the SQL fuzzer found and fixed a non-idempotent tokenizer bug), and a nightly fuzz CI accumulates the corpus past the PR seed set |
| Silence-is-never-a-silent-pass | ✅ | Fail-closed conventions are *tested*: unknown statuses → caution, inert rules → UNMATCHED, dead exceptions → flagged, blind frontiers → caution/require_proof; an unmarshalable span signature fails closed (panics) rather than degrading to op-only order; gate matchers bind at identifier boundaries so a prefix collision can no longer fail open (`policy.MatchPrefix`, class-guarded by `opkey.TestNoHardcodedOpKeyPrefix`) |
| No AI in any verdict | ✅ | By construction; E4 deliberately excluded from the suite for this reason |
| Documentation | ✅ | Concepts primer, integration guide, drill record, this scorecard; every doc claim maps to a runnable command and a locking test |

## ⚠️ Unproven — graded honestly as such

| Question | Status |
|---|---|
| **Behavior at scale** (10⁵-node graphs, interface-heavy monoliths) | 📐 First real data point (2026-06-13): an 891-node / 107-HighFanOut service ran the CX engine with **no measurable overhead (~2s, OFF ≈ ON)** and **trust monotonicity held** (only VIOLATED→CANT-PROVE, never a new VIOLATED). Two honest limits it exposed: the interprocedural lifts abstain at HighFanOut chokepoints (their value is gated by dispatch precision, not soundness — see correctness-expansion-plan D-CX10), and a `require_proof` rule with an unbindable third-party sink reported HOLDS vacuously (fixed). Still ⚠️ above ~10³ nodes; the 10⁵ monolith remains unmeasured. |
| **E4: does an agent actually do better with these tools?** | 📋 Designed with criteria and a results slot in `drills.md`; needs live human-judged sessions. Until run, "net positive for the agent" is a structural argument, not a measurement. |
| **External adoption / sustained use** | ⚠️ Zero adopters outside the dogfood fixture. The behavioral pipeline's authoring cost in particular has no field evidence. |
| **Behavioral impeachment in the field** | ⚠️ The mechanism is ✅ (locked, proven E2E) — but on **one** fixture (`impeachsvc`). It has never run against a diverse real corpus, so how often it finds a *real* missed edge (vs. produces only abstaining downgrades) is unmeasured. Its worst case is abstention, not a false impeachment, so the soundness risk is low; the **value** is the unproven part. The honest next step is running it against several third-party Go services and publishing where the verdicts landed, including the abstains. |
| **Cross-service triage** | ⚠️ Per-service only; the contract diff and system rendering exist, but an incident walk across service boundaries does not. |
| **Maintenance bus factor** | ⚠️ The obligations SSA analysis is subtle (the adversarial review found six semantic bugs in its first version — all fixed and locked, but the subtlety remains). It needs more than one fluent maintainer. |

## Standing residuals (decided, not pending)

Version-skew decode failures are the documented lockstep design; render-text
drift is uncommitted presentation; Gate/Review blind-spot asymmetry is
intentional; dynamic deferred values in recover detection are accepted
(abstaining would abstain on `defer cancel()` everywhere); the obligations
(pkg, name) site-bucketing waits for profiling evidence. Each was pressure-
tested and chosen, with the reasoning recorded in `review-fixes-plan.md`.

## The one-line summary

**Everything buildable from inside this repo is built, locked, and where
possible measured; the claims that matter most to a real adopter — scale,
agent benefit, sustained adoption, and the field *value* (not soundness) of
behavioral impeachment — are exactly the ones that cannot be proven from inside
this repo, and they are graded accordingly.**
