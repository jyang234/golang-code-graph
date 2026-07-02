# Audit remediation plan — 2026-07

> **`PLANNING`** · execution plan for `docs/audit/2026-07-code-quality-audit.md`
> · ordered by criticality · baseline commit `9450868`, `make verify` green
>
> This is a **tracking plan**, not code. It reorganizes the audit's findings
> into an execution order, records dependencies between findings, and pins the
> regression-test shape each fix must ship with (per CLAUDE.md). No toolchain
> code is changed by this document. Check items off as they land.

## How to use this document

- Work top-to-bottom by phase. Each phase is independently shippable and must
  leave `make verify` **green** at its end (and, ideally, after every finding).
- Every finding links back to its full write-up in the audit by ID
  (`C-1`, `H-4`, `M-2`, …). This plan does **not** restate the defect — read
  the audit entry before starting a finding.
- The **Test** column names the regression the fix must ship with. A soundness
  fix ships a test that exercises the *exact reproduced shape*; an
  ordering/canonicalization fix ships a determinism test or a canon-fuzz-corpus
  extension. This is a CLAUDE.md requirement, not optional.
- **Coupled** findings must be fixed in one change-set (fixing one alone leaves
  a latent or newly-live bug). They are grouped into a single row.
- Do not "fix" anything in audit §6 (verified strengths) or the documented
  standing residuals in `docs/groundwork/scorecard.md`.

## Status legend

`[ ]` not started · `[~]` in progress · `[x]` landed (fix + test, verify green)

---

## Phase 0 — stop the bleeding (small, high-leverage) — DETAILED

The first increment. Every item here is small, self-contained, and closes a
live crash / fail-open / trust-boundary gap with minimal blast radius. This is
the phase to implement first once planning is approved.

| # | ID(s) | What | Where | Test to ship |
|---|---|---|---|---|
| 0.1 | **M-2** | CI must mirror `make verify`: add impeachsvc fixture to CI; add `-race` to `make test` (or a `test-race` target `verify` includes); align Makefile gofmt exclusion to cover impeachsvc; pin golangci-lint version local↔CI; reconcile Go version split (go.mod 1.24.0 / go.work 1.25.0 / CI 1.25.11). | `.github/workflows/gates.yml:43`, `Makefile:32-33` | CI change is self-verifying; confirm CI runs both fixtures + `-race`. |
| 0.2 | **H-4** | Reproducible nil-deref panic: a first-party pkg under `classify.db` crashes `Build` for every reachable node. Add nil-site guards in `StringArgs`/`constSQLOp`/`sinkMethodName` (fail closed to dynamic/method-name label) and/or skip the hint switch for `nodeTier`'s synthetic self-edge. | `graphio/graphio.go:1350`, `features/features.go:75,153-182`, `features/hints.go:236-244` | Fixture: first-party package under `classify.db`; `Build` returns, no panic. |
| 0.3 | **H-14** | `Flow.Tier()` public API silently degrades unknown strings to `warn`. Validate against `warn\|info\|debug\|all` in `Run` (fatal on unknown), sourcing the set from one shared place with config. | `flow/flow.go:124`, `config/config.go:664-669` | Both paths: `Tier("Info")`/typo fails loudly; valid names pass. |
| 0.4 | **M-33 (no-op only)** | `behavior ingest --update` without `--flows-dir` is a silent no-op — error out. (Rest of M-33 CLI ergonomics deferred to Phase 4.) | `cmd/flowmap/main.go:843-857` | Test: `--update` without `--flows-dir` exits non-zero with a clear message. |
| 0.5 | **M-15** | `graph.Load` silently ignores trailing data after the first JSON value. Check `dec.More()`/EOF and refuse a truncated/concatenated graph file. | `graph/graph.go:509-520` | Test: concatenated/trailing-garbage graph file is rejected. |

**Phase 0 exit:** `make verify` green; CI green on both fixtures with `-race`.

---

## Phase 1 — soundness criticals (false-verdict paths)

Each item = fix **plus** a regression test shaped like the audit's reproduced
PoC. These are the wrong-verdict findings — the repo's named worst-outcome
class. Order within the phase is by blast radius; C-1 is the largest single
work item.

| # | ID(s) — coupled | What (short) | Where | Test to ship |
|---|---|---|---|---|
| 1.1 `[x]` | **C-1 + M-20** | ONE function-level first-party predicate (`fn.Pkg` → `Origin().Pkg` → `Object().Pkg()`) used in `firstPartyScope`, `blindspots`, `taint.firstPartyFuncs`; render/splice nil-`Pkg` synthetic nodes (generic instances, `$bound`/`$thunk`); derive `Node.Package`/`File` for them; **and** the callgraph FQN sort tie-break (M-20 goes live the moment C-1 lands). Restore the stale `graphio.go:262-267` comment. | `graphio/scope.go:24-35,138-160`, `graphio.go:1148-1151`, `ssabuild.go:61-67`, `blindspots.go:309`, `taint.go:693-703`, `callgraph.go:171-199` | Fixtures: boundary effect (a) behind a method-value callback through a `*T` receiver, (b) inside a first-party generic helper — assert node+edge present in `Build` output and a `must_not_reach` over the seam finds the path. Determinism test for the FQN tie-break on non-unique instance names. |
| 1.2 `[x]` | **C-2** | obligations false SATISFIED on reassigned `err`. Restrict failure-branch pruning to the acquire's error result before any redefinition; abstain (CANT-PROVE) when the acquire error flows into a multi-store alloc. | `obligations.go:658-707` | Regression mirroring the `LeakAfterReassign` PoC → not SATISFIED. Mind the M-14 `usesRecover` interaction (both guard SATISFIED). |
| 1.3 `[x]` | **C-3 + C-4 + C-5 + M-13** | taint one change-set: escape on invoke-mode calls with un-enumerable concrete callees; index `callsTo`/source-seeding over resolved callees (RTA/VTA in hand) or escape the invoke result; propagate taint to the struct alloc on field store. Add M-13's `--strict` (fail on ABSTAIN) + loud gate-mode warning. Temper the by-source/by-sink doc comments. | `taint.go:403-524,445-503`; `cmd/flowmap/main.go:550-555` | The three PoC shapes (interface-return taint; declared-source invoked via interface; struct-carried field to unmodeled callee) all → ESCAPE/ABSTAIN, not NO-FLOW. `--strict` exits non-zero on ABSTAIN. |
| 1.4 `[x]` | **C-6** | `no_concurrent_reach` blind probe: extend to any `ConcurrentDispatch` blind spot graph-wide and to `direct` edges with `IsDynamic()`. | `fitness/concurrent.go:32-51` | ConcurrentDispatch spot + zero concurrent edges + `require_proof:true` → caution/violation, not silence. |
| 1.5 `[x]` | **C-7 + M-26** | promote: add explicit `Unordered` branch symmetric with `Concurrent` (per-member keep/promote, re-sort, `Unordered: len>1`); assert the sequential single-member invariant (fail closed); propagate `Multiplicity` onto spliced children (M-26); fix the `:76` comment. | `promote/promote.go:76-84,83-84`, `canon.go:372` | Promote unit tests (unordered keep/drop/promote) + end-to-end post-hoc regression with a droppable member ordered before a tier-1 member. |
| 1.6 `[x]` | **C-8 + M-1** | capture `Concurrent`: require `a.Goroutine != b.Goroutine` in the structural branch; update the doc comment. Fold in M-1 (`goid()!=0` self-check failing loudly through `TB`; soften the comment). | `capture/capture.go:277-286`, `harness/goid.go:16-19` | `a.Goroutine == b.Goroutine != parent` must not be concurrent; goid self-check fires on parse failure. |

**Phase 1 exit:** every wrong-verdict path in the audit closed; `make verify`
green. Consider the two "shape fixture" services (audit §7.2) as permanent
`go.work` members here.

---

## Phase 2 — fail-open HIGHs (funnel must disclose or refuse)

| # | ID(s) — coupled | What (short) | Where | Test to ship |
|---|---|---|---|---|
| 2.1 `[x]` | **H-1 + M-24 + M-25** | SQL normalizer hardening: consume `\` inside quoted literals (H-1); strip comments + treat `"`/backtick as identifier quoting, lower-cased (M-24); lower-case attribute-sourced table (M-25). | `canon/sql/sql.go:45-107,179-189`, `opkey.go:258-278` | **Leak-property fuzzer**: no byte from inside a quoted literal survives into `Normalized.Statement` (stronger than idempotence). Extend fuzz corpus. |
| 2.2 `[x]` | **H-2** (+ dup-ID LOW) | Reachability completeness: compare spans-reachable-from-root + orphan heads against `len(cf.Spans)`; refuse (`ErrIncomplete`) on mismatch (parent cycles, duplicate-ID shadowing). | `canon/canon.go:59-81,516-538,61-64` | Root + two spans in a 2-cycle carrying DELETE → refused, not silently empty. |
| 2.3 `[x]` | **H-3** | Allowlisting `db.statement`/`db.query.text` re-injects raw SQL. Skip already-projected keys in the allow loop, or route them through the SQL normalizer. | `canon/canon.go:472-497` | Pin "allowlisted `db.statement` is still normalized". |
| 2.4 `[x]` | **H-5** | `bindsAnyTarget` uses `e.IsBoundary()` (prefix), matching every other consumer; `graph.Load` rejects `IsBoundary() != (Boundary != "")` (fail-closed decoder invariant). | `fitness/reach.go:112-124`, `graph/graph.go:499` | Parity test: `boundary:` target with empty `Boundary` field yields a real violation, not a "binds nothing" caution. |
| 2.5 `[x]` | **H-6** | Hoist ONE shared exception matcher with the `pat == "" \|\|` wildcard clause; used by `exempted` and `policy.Allowed`. | `fitness/layering.go:114-121`, `policy/policy.go:175-185` | One-sided layering entry over a free-function edge and a method edge both behave. |
| 2.6 `[x]` | **H-7** | Add non-blocking `NewCautions []Violation` to `GateResult` (digest-covered); render on PASS and BLOCK. | `review/gate.go:113` | Mirror `TestGateSurfacesStandingCautionOnPass` for new cautions. |
| 2.7 `[x]` | **H-11** | sqlfold: fail closed on any call inside the accumulator method that can touch the receiver's builder and can't be classified (builder/receiver into a non-modeled call = hole → abstain). | `sqlfold/summary.go:66-118` | `Fprintf`-splicing accumulator → not a complete constant SELECT. |
| 2.8 `[x]` | **H-12** | schemadrift: teach `stripSQL`/`splitStatements` dollar-quoting (`$tag$…$tag$`) — strip such bodies, or emit a fail-closed `ParseCaveat` on any `$…$` delimiter. | `schemadrift/migrations.go:202-241,262-283` | `CREATE TABLE` inside a plpgsql body no longer masks drift (test both ways). |
| 2.9 `[x]` | **H-13** | otlpjson snake_case: either extend snake_case tolerance uniformly or drop envelope tolerance so a full snake doc fails `errNotOTLP`. Independently, make the ingest gate fail loudly when a flows dir has committed goldens but zero flows decoded. | `otlpjson/otlpjson.go:272-302`, `cmd/flowmap/main.go:788-796` | Full snake_case document test; loud failure on goldens-present-zero-decoded. |

**Phase 2 exit:** disclosure/redaction funnels either disclose or refuse;
`make verify` green. **Phases 0–2 together close every wrong-verdict path the
audit identified.**

---

## Phase 3 — drift closure (one-source-of-truth debt)

The structurally important phase: collapse each parallel surface to one home so
this audit's largest class cannot recur silently. Prefer self-checking parity
tests (the `TestNoHardcodedOpKeyPrefix` / `TestVerbParity` pattern).

| # | ID(s) — coupled | What (short) | Where | Test to ship |
|---|---|---|---|---|
| 3.1 `[x]` | **H-8 + M-9** | ONE io_budget root predicate: `fitness.IsRoute(p, ix, fqn)` used by the ground card and enforcer; `RouteWrites` falls back to `CompositionRoots()` when `RootPackages()` empty; proposers call the enforcer's predicate. | `ground/ground.go:164`, `fitness/budget.go:14-22,135-141`, `fitness/propose.go:439-476,645-653` | Root-package source: card ↔ enforcer agree; property-test generator gets `.main` roots. |
| 3.2 `[x]` | **H-9 + H-10** | ONE caveat-assembly helper feeding CLI fitness, SARIF, and MCP fitness; MCP prefixes `ProvenanceLine` + per-finding witness lines; add `SQLFoldCaveat` to fitness/SARIF and split it by via kind. | `cmd/groundwork/mcp.go:818-833`, `main.go:571-596,577-583`, `review/provenance.go:43`, `graph.go:420-436` | MCP-parity test mirroring `TestSARIFCarriesCaveats`; folded fixture. |
| 3.3 `[x]` | **M-4** | Shared broker-conflict merge helper (`policy.MergeBrokers`), sorted conflict names, used by CLI + MCP. | `main.go:512-521`, `mcp.go:777-790` | Two conflicting brokers → deterministic text both surfaces. |
| 3.4 `[x]` | **M-7** | Hoist `"boundary:db "`/`"boundary:bus "` label grammar into a shared package (alongside `effectkind`/`opkey`); extend the prefix-guard repo-scan test. | ~10 packages | Repo-scan guard test à la `TestNoHardcodedOpKeyPrefix`. |
| 3.5 `[x]` | **M-8** | Named parity test for the obligation verdict vocabulary (producer vs consumer). | `obligations.go:56-61`, `fitness/obligations.go:13-18` | Direct parity test like `TestVerbParity`. |
| 3.6 `[x]` | **M-10** | Export one `opkey.DBOperation(attrs)` used by both derivation sites. | `opkey.go:259-269`, `canon.go:595-609` | Parity comment + test. |
| 3.7 `[x]` | **M-11** | Shared `triageCard(ix, symptom)`; converge or explicitly rename the two `reach` renders. | `main.go:202-274`, `mcp.go:859-896` | Parity/behavior test for the shared card. |
| 3.8 `[x]` | **M-12** | Share ONE `--entry` resolver; `Build` errors on ambiguity. | `scope.go:127-134`, `mermaid_rooted.go:153-168` | Ambiguous `--entry` fails closed. |
| 3.9 `[x]` | **M-27** | `contract.Compare` refuses on `base.Service != branch.Service`. | `contract/contract.go:111-117` | Mixed-service compare → refused. |
| 3.10 `[x]` | **M-28** | `changedFns` marks `e.From` changed for base edges absent from branch; correct the doc. | `reviewtriage.go:426-450` | Body change that removed an auth-check call appears in a triage zone. |
| 3.11 `[x]` | **M-29** | io_effects delta dedupe on `(Op, Effect, Write)` dropping cancelling pairs (inherit the contract-surface R10 fix). | `review.go:370-385`, `delta.go:169-181` | Pure emitter move → no duplicate `+`/phantom `-` rows. |

**Phase 3 exit:** each drifted predicate has one home + a parity guard; `make
verify` green.

---

## Phase 4 — hardening & disclosure parity (remaining MEDIUM + LOW) — `LANDED`

Batch-friendly. Group by theme; each is self-contained. All items below landed
(fix + test, `make verify` green).

**Determinism residues:** M-3 `[x]` (span-ID tie-break → intrinsic key: op key
then subtree signature; equal-start unorderable siblings fold to an Unordered
group; `FuzzCanonSiblingOrderInvariant` added), M-5 `[x]` (Mermaid fold merge
sorted), M-22 `[x]` (`FindFunc` collect-all + panic on genuine ambiguity), M-23
`[x]` (reclaim sort `Pos()` tie-break).

**Fail-open / disclosure gaps:** M-14 `[x]` (disclosure stays accurate post-C-2;
no code change beyond C-2), M-16 `[x]` (reject edges whose `From` is not a node
at `graph.Load`), M-17 `[x]` (io_budget zero-route vacuity caution), M-18 `[x]`
(disclose correlation-less in-window spans via `CapturedFlow.CorrelationLess`),
M-19 `[x]` (document fire-and-forget marker requirement), M-30 `[x]` (impeach
`canonicalDigest` fails loud, matching its review sibling), M-35 `[x]` (roots
non-constant-route drop → blind spot + fixed comment).

**Process / CI:** M-21 `[x]` (nightly fuzz enumerates all 14 targets + H-1's
leak fuzzer; `internal/fuzzcov` parity guard), M-6 `[x]` (reflection-based
digest field-coverage self-check for `Artifact`/`GateResult`).

**Public API / fidelity:** M-31 `[x]` (harness maps span links/TraceID), M-32
`[x]` (`statusRecorder` `Unwrap`/`Flush` passthrough), M-34 `[x]` (MCP session-id
continues past an appended log's high-water mark), rest of **M-33** `[x]`
(flowmap `-h` clean exit via `flag.ErrHelp`; help/flag parity + parity tests;
usageBody `--expect` prose relocated, omitted flags documented).

**LOW list (audit §5):** stale-comment fixes `[x]`, robustness/fail-closed
polish `[x]` (harness rand fail-loud, `SortBlindSpots` total order, policy
reach dup-name guards, `BusEffects` malformed tally, ground shared `DedupKey`,
coverage peerless-HTTP guard, config pin-identity/version guards), CLI/server
polish `[x]` (MCP token SHA-256 constant-time compare + Bearer scheme + server
timeouts, stdio `-32700` parse error). A few narrow §5 residuals (CaptureMode
zero-value refusal — deferred as too disruptive for its LOW value; both
production callers already set it) are left documented rather than forced.

---

## Cross-cutting structural work (audit §7 tail)

Fold these into the phases above rather than doing them separately:

1. **Self-checking parallel-surface invariants** (do as part of Phase 3):
   CLI↔MCP disclosure parity, caveat-assembly parity, boundary-label prefixes,
   obligation-status strings, usage-text↔flag-set parity — each as a repo-scan
   or reflection parity test so the drift class is structurally recurrence-proof.
2. **Projection-layer "shape fixtures"** (do as part of Phase 1): add the two
   shapes that hid C-1 (effect behind a method-value wrapper; effect inside a
   generic instance) as permanent fixture services in `go.work`, the way
   `mwchainsvc`/`reflectsvc` pin the reclaimer poles.

---

## Dependency / sequencing notes

- **M-20 is latent until C-1 lands**, then live — fix in the same change-set (1.1).
- **M-13** rides with the C-3/C-4/C-5 taint change-set (1.3): escapes become the
  common outcome, so the gate's ABSTAIN posture becomes operative there.
- **M-26** and the promote comment fix ride with C-7 (1.5).
- **M-1** rides with C-8 (1.6) — both touch the goroutine-identity claim.
- **H-9 and H-10** share one caveat-assembly helper (3.2) — do together.
- **H-8 and M-9** share one root predicate (3.1) — do together.
- **H-1, M-24, M-25** are one SQL-normalizer change-set (2.1); the H-2 dup-ID
  LOW folds into H-2's completeness check (2.2).
- Do **not** relitigate audit §6 strengths or `scorecard.md` standing residuals.
