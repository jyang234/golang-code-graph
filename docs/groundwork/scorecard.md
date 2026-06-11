# Capability scorecard — an honest assessment

**As of:** 2026-06-11, branch `claude/design-ideas-planning-0ymckn`.
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

## groundwork (the judge)

| Capability | Grade | Evidence | Known limits |
|---|---|---|---|
| Six policy families (layering, must_not_reach, must_pass_through, no_concurrent_reach, io_budget, blind-spot ratchet) | ✅ | Each family has fixture verdicts both ways; `entrypoint:*` binds everywhere; fail-closed on unknown vocabularies | `concurrent` flag conflates go/defer (disclosed; split planned on evidence); path-insensitive over-approximation throughout |
| Review artifact + digest + verify-artifact | ✅ | Tamper, stale, and **re-signed forgery** all caught by tests; abstention suppressed by any new signal | Digest is not the anchor — recomputation from CI-generated graphs is; set-based delta misses new-call-site-to-already-called-target (documented) |
| Pre-flight gate (`verify`) | ✅ | Blocks on new violations / scope creep / breaking contract / gated blind spots; fixture-proven | Only as trustworthy as graph generation — the trust boundary is CI wiring, not this binary |
| Exceptions audit (dead-entry detection) | ✅ | Set-based attribution; the caution↔violation swap that fooled the count-proxy version is the regression test | Liveness is per-graph: an entry dead on this service may be live on another sharing the policy |
| Incident triage (5 symptom kinds) | 📐 | **10/10 recall, median 8% hunt space, route scenarios 3%** (E1); trace handoff proven end-to-end (E2); staleness mis-scope demonstrated (E3); thresholds are committed assertions | Fixture is 39 nodes and well-factored — fractions will grow on monoliths (why the thresholds are ratchets); non-code causes are out of scope, stated on every fault card |
| Partial-effect fault answers | ✅ | Certainly/possibly split locked; scope statement prints even when sections are empty | Inherits effect_order's same-function limit |
| Ground cards (pre-edit binding rules) | ✅ | The defining test seeds the violation the card warns about and asserts the named rules fire; same matchers as the checks | Binding ≠ exhaustive: only declared rules appear; an unconfigured hazard is invisible by definition |
| MCP server | ✅ | Scripted-session tests: handshake, discovery, cards, isError tool results, -32601; fleet session: prefixed entrypoints, fleet-events join, explicit-hop errors; HTTP session: bearer auth, Origin rejection, 405/202/400 transport discipline, fail-closed exposure guard | Staleness flagged but reload is manual by design; fleet-events covers loaded services only; HTTP auth is one static bearer token (TLS/identity belong to a reverse proxy); no SSE streams or sessions (stateless by design); first-of-kind surface with no field hours |
| Effectiveness drills as ratchet | 📐 | E1–E3 committed; numbers reprint on every `-v` run | They measure that triage does its job well, not that its job covers everything |
| Transcript instrument (`--log` + `transcript`) | ✅ | Byte-exact log-format test; summary semantics (sessions, hops through fleet-wide calls, corrections) locked by unit tests; strict decode fails closed on unknown lines | Counts measure usage, not value — E4's qualitative half (do conclusions cite card facts?) stays human-judged; no E4 field data yet |

## Cross-cutting properties

| Property | Grade | Evidence |
|---|---|---|
| Byte-determinism across machines | ✅ | Cross-checkout path-invariance test; canonical JSON everywhere; sites normalized through a total ladder |
| Silence-is-never-a-silent-pass | ✅ | Fail-closed conventions are *tested*: unknown statuses → caution, inert rules → UNMATCHED, dead exceptions → flagged, blind frontiers → caution/require_proof |
| No AI in any verdict | ✅ | By construction; E4 deliberately excluded from the suite for this reason |
| Documentation | ✅ | Concepts primer, integration guide, drill record, this scorecard; every doc claim maps to a runnable command and a locking test |

## ⚠️ Unproven — graded honestly as such

| Question | Status |
|---|---|
| **Behavior at scale** (10⁵-node graphs, interface-heavy monoliths) | ⚠️ Nothing measured beyond the fixture. Expect: larger cones, more HighFanOut blind spots, growing hunt fractions. The committed thresholds will detect erosion; they cannot prevent it. **The single highest-leverage next step is one real, large service.** |
| **E4: does an agent actually do better with these tools?** | 📋 Designed with criteria and a results slot in `drills.md`; needs live human-judged sessions. Until run, "net positive for the agent" is a structural argument, not a measurement. |
| **External adoption / sustained use** | ⚠️ Zero adopters outside the dogfood fixture. The behavioral pipeline's authoring cost in particular has no field evidence. |
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
possible measured; the three claims that matter most to a real adopter —
scale, agent benefit, sustained adoption — are exactly the three that cannot
be proven from inside this repo, and they are graded accordingly.**
