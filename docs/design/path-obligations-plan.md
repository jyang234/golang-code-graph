# path obligations: implementation plan

**Status:** implemented — OB-0 through OB-3 all shipped. Kept as the design
record (the must-release correction in §4, D-OB1/5/6). Refined from
[`ideas/path-obligations.md`](ideas/path-obligations.md). Grounded in the actual
code (file references verified), not just the idea doc.

Scope decisions made when this plan was cut:

- **D-OB1 — both obligation kinds in slice 1**: `must-release` *and*
  `must-precede`. They share all infrastructure (config, analysis package,
  graph.json section, fixture, groundwork plumbing), and `must-precede` is the
  dominance machinery the incident-triage plan's partial-effect check depends on
  (see [`incident-triage-plan.md`](incident-triage-plan.md), Phase IT-3).
- **D-OB2 — rules live in `.flowmap.yaml`**: evaluation stays where the SSA
  lives (flowmap, the trusted CI side); the rules ride the same
  CODEOWNERS-gated document as the classify hints. groundwork never sees rules,
  only findings.
- **D-OB3 — sequencing**: this lands before the incident-triage work.
  *Amended by D-GX1* ([`guardrail-extensions-plan.md` §7](guardrail-extensions-plan.md)):
  the zero-dependency GX-2/GX-1 checks ship first; this track follows
  immediately after and still precedes incident triage.
- **D-OB5 — the `obligations` section is the generic findings envelope.** Its
  shape `{rule, kind, fn, site, status, detail}` carries *any* future
  flowmap-side (SSA/CFG-level) check: `kind` is an open registry, not an enum
  of two. A new check kind is therefore flowmap analysis + a registered kind —
  **zero graph.json schema changes**, so the strict-decode lockstep tax (§2) is
  paid once, here, not per future check.
- **D-OB6 — finding identity is the site, never the prose.** The
  base-vs-branch diff keys findings on `Rule + From + To + Summary`
  (`internal/groundwork/review/review.go:59-84`). Obligation findings put the
  fn FQN in `From`, the `file.go:line` site in `To`, and build `Summary` only
  from key fields (`rule: status`); free text lives in `detail` and never
  enters the key. This is framework policy for all future check kinds: a
  wording tweak must never make old findings look "new" — that would silently
  break the ratchet.

Sibling extensions that ride this machinery but live graph-side (call-graph
level, no SSA) are planned separately in
[`guardrail-extensions-plan.md`](guardrail-extensions-plan.md).

---

## 1. A correction to the idea doc

The idea doc calls both checks "dominator-tree queries". Only one is:

- **must-precede** ("every B site is dominated by an A site") *is* a pure
  dominance query — `ssa.BasicBlock.Dominates` / the dominator tree SSA already
  computes.
- **must-release** ("after an acquire, every path to exit hits a release") is
  **not** dominance; it is a forward CFG reachability question: *is any function
  exit reachable from the acquire block without passing a release site?* — with
  `defer`-crediting (a release registered via `defer` before the path covers
  every later exit). Same substrate (`ssa.Function.Blocks`), same determinism,
  different walk.

Both remain simple, deterministic, and intraprocedural. This changes
implementation shape only, not feasibility or cost.

## 2. The interface (verified facts)

- flowmap already builds full SSA: `internal/static/ssabuild/ssabuild.go:30-36`
  (`ssautil.Packages(..., ssa.InstantiateGenerics); prog.Build()`); every
  call-graph node carries its `*ssa.Function`
  (`internal/static/callgraph/callgraph.go`). The CFGs exist; nothing new is
  constructed.
- `.flowmap.yaml` is loaded with `KnownFields(true)` and a `validate()` pass
  (`internal/config/config.go:206-256`) — a typo'd obligation key fails loudly,
  matching the existing discipline.
- graph.json is emitted by `internal/static/graphio/graphio.go` and decoded by
  groundwork with `DisallowUnknownFields`
  (`internal/groundwork/graph/graph.go:84-95`). **A new `obligations` section is
  therefore a lockstep change**: graphio emit + groundwork decode + golden
  regeneration (`testdata/groundwork/regen.sh`) land in the same commit, which
  is exactly what the strict decoder is for.
- groundwork findings are two-severity (`Violation` fails the gate, `Caution` is
  the legible abstention — `internal/groundwork/fitness/result.go:9-24`), and
  `review`/`verify` already diff findings base-vs-branch by key and report only
  new ones (`internal/groundwork/review/review.go:59-84`). Obligation findings
  flow through unchanged.

## 3. Config schema

Extend `Config` (`internal/config/config.go`) with an ordered slice (never a
map):

```yaml
obligations:
  - name: tx-must-close            # required, unique
    acquire: "example.com/svc/internal/store#BeginTx"
    release:
      - "example.com/svc/internal/store#Commit"
      - "example.com/svc/internal/store#Rollback"
  - name: audit-before-publish
    require: "example.com/svc/internal/audit#Write"
    before:  "example.com/svc/internal/eventbus#Publish"
```

- Kind is inferred from the fields: `acquire`/`release` ⇒ must-release,
  `require`/`before` ⇒ must-precede. Mixing the two field sets in one rule is a
  validation error, as is an empty `release` list or a missing `name`.
- FQN syntax is the existing `import/path#Symbol` form the classify hints use —
  one notation across the document.

## 4. The analysis: `internal/static/obligations`

A new package, pure function of (`*ssa.Function`, rules) → findings. No global
state, no interprocedural reasoning in slice 1.

**Site discovery.** For each function in the call graph, scan call instructions
for acquire / release / require / before targets (matching the rule FQNs the
same way the classify-hint matcher does). **Rule FQNs match both concrete and
interface-method call targets**: an invoke-mode call through an interface whose
method FQN matches counts as a site, and a rule anchored on the interface
method matches the concrete implementations' call sites resolved by the call
graph. Acquiring through an interface is idiomatic Go — without this, the
guardrail silently misses the common case; a unit-test row covers each
direction. Functions with no relevant site produce nothing — the common case,
so cost is near zero on rule-free services.

**Dead-rule disclosure.** A rule whose anchor (`acquire` / `before`) matches
**zero call sites** across the whole service emits a single
`{rule, kind, status: "UNMATCHED"}` entry (fn/site empty). A rule that
silently stopped matching after a rename is a guardrail that fell off without
anyone noticing — the same failure mode as a test that stopped running, and
the human reviewer must be able to see that the rules they believe are
protecting them actually bind. flowmap is the only side that knows the rules,
so the disclosure is emitted here and judged by groundwork (§6).

**must-release.** For each acquire site: walk forward over `Blocks` from the
acquire instruction; a path is *covered* if it passes a release call or a
`defer` of a release that was registered before the exit. If a function exit
(`return`, explicit `panic`) is reachable uncovered → `VIOLATED`, with the
uncovered exit's position as the witness. Implicit runtime panics are ignored
(standard practice for lifecycle checkers — `sqlclosecheck` does the same).

**must-precede.** For each `before` (B) site: `SATISFIED` iff some `require` (A)
site dominates it (A in a dominating block, or earlier in the same block).
Otherwise `VIOLATED`, naming the undominated B site.

**Abstention (`CANT-PROVE`) triggers — disclosed, never silent (refined during
implementation):**

- the acquired value's **ownership leaves the function**: returned, stored,
  captured by a closure, or handed to a goroutine. Plain argument passing is
  deliberately NOT an escape — the original draft abstained on it, but that
  contradicts the worked example (`debit(tx, …)` must still yield VIOLATED)
  and would abstain on essentially every real transaction. The check is
  value-blind: a release performed inside an unlisted helper reports VIOLATED,
  and the fix is naming the helper as a release ref — the rule vocabulary is
  the mechanism.
- `recover` is present in the function or a deferred closure (control rejoins
  invisibly).
- ~~the CFG is irreducible~~ — dropped: the forward walk does not rely on
  reducibility, and SSA's dominator tree is defined for any CFG, so
  irreducibility needs no abstention.

Two further refinements the implementation fixed in place: the acquire's own
failure branch (`if err != nil { return err }` testing the acquire's error
result) is pruned — a failed acquire holds nothing, so that return is not a
leak; and the resource being tracked is the acquire's non-error result
components only, so `return err` on the failure path is not "the resource
escaping".

**Determinism.** Findings sorted by (rule, fn FQN, site position). Positions
are `file.go:line` from the SSA instruction's FileSet — and this is the **first
time source positions enter graph.json**, so they carry a normalization
requirement the rest of the schema never needed: the FileSet returns
*absolute* paths, which differ between CI and local checkouts and would break
byte-identical output across machines. Sites are emitted **module-relative
with forward slashes**, verified by a test that runs the pipeline from two
different parent directories and asserts identical bytes. With that, the
artifact digest stays stable — same property the rest of the pipeline already
guarantees.

## 5. graph.json: the `obligations` section

A separate, FQN-keyed disclosure section (the sanctioned "narrow level-2 slice"
form), not folded into call-graph edges:

```json
"obligations": [
  {"rule": "tx-must-close", "kind": "must-release",
   "fn": "example.com/svc/internal/store.Transfer", "site": "store.go:42",
   "status": "VIOLATED", "detail": "exit at store.go:48 reachable without release"},
  {"rule": "audit-before-publish", "kind": "must-precede",
   "fn": "example.com/svc/internal/app.Disburse", "site": "app.go:91",
   "status": "SATISFIED"}
]
```

**D-OB4 — emit all three statuses, including SATISFIED.** SATISFIED is the
valuable universal ("no modeled path leaks") and emitting it is what makes the
drift ratchet visible: a SATISFIED→VIOLATED flip shows up in the
base-vs-branch diff as a new violation, and the proof's existence is auditable
rather than implied by silence. The section is omitted entirely (`omitempty`)
when no rules are configured, so rule-free services see a byte-identical
graph.json.

## 6. groundwork: judging

- `graph.Graph` gains the `Obligations []Obligation` field (lockstep with
  graphio, §2).
- `fitness.Check` gains `checkObligations`: `VIOLATED` → `Violation`,
  `CANT-PROVE` → `Caution`, `UNMATCHED` → `Caution` ("rule matches nothing —
  inert guardrail"), `SATISFIED` → no finding. Finding key = (rule, fn, site)
  per D-OB6, so `review`/`verify`'s new-findings diff and the three-valued
  verdict need **zero changes**.
- A future `require_proof` escalation (CANT-PROVE → gate-failing for named
  rules) is policy-side and deliberately out of slice 1.

## 7. Fixture and tests

New fixture `testdata/groundwork/obligsvc` (sibling of `layeredsvc`/`blindsvc`),
with its golden graph wired into `regen.sh` and `cmd/groundwork/main_test.go`.
It exercises one case per verdict per kind:

| function | shape | expected |
|---|---|---|
| `Transfer` | acquire, error-return path with no release | must-release `VIOLATED` |
| `TransferDefer` | acquire + `defer Rollback()` | must-release `SATISFIED` |
| `TransferOwn` | acquire, the open tx is returned | must-release `CANT-PROVE` |
| `Disburse` | audit dominates publish | must-precede `SATISFIED` |
| `DisburseRacy` | publish on a branch with no audit | must-precede `VIOLATED` |
| *(config)* | rule anchored on a renamed-away FQN | `UNMATCHED` |

Unit tests in `internal/static/obligations` build SSA from inline source
snippets (table-driven), covering defer ordering, multi-exit, loops,
release-on-one-arm, and each abstention trigger.

## 8. Build order

- **Phase OB-0 — config.** `obligations:` schema + validation in
  `internal/config`. *Exit: malformed rules rejected with positional errors;
  valid rules round-trip.*
- **Phase OB-1 — analysis.** `internal/static/obligations` with both walks
  plus dead-rule detection, unit-tested against inline-SSA tables. *Exit:
  every fixture-shape above (including `UNMATCHED`) verdicts correctly at the
  package level.*
- **Phase OB-2 — emission, lockstep.** Wire into
  `internal/static/analyze/analyze.go`; graphio emits `obligations`; groundwork
  decodes it; goldens regenerated. One commit. *Exit: `flowmap graph obligsvc`
  emits the section deterministically; groundwork loads it; rule-free services'
  graphs are byte-identical to before.*
- **Phase OB-3 — judging.** `fitness.checkObligations`; end-to-end
  `review`/`verify` on obligsvc base-vs-branch (introduce the leak in a branch
  fixture → BLOCK with exactly one new violation). *Exit: the worked example
  from the idea doc reproduces end-to-end.*

## 9. Verifiable outcomes and validation

**Landed correctly — deterministic, machine-checked (CI):**

- **O1 — verdict correctness.** Every obligsvc fixture shape (§7, including
  `UNMATCHED` and both interface-matching directions) produces exactly its
  expected verdict, at the unit level and through the golden graph.
- **O2 — determinism.** graph.json with obligations is byte-identical across
  repeat runs *and* across two different checkout paths (the
  position-normalization test, §4).
- **O3 — zero-impact guarantee.** A rule-free service's graph.json is
  byte-identical to pre-feature output.
- **O4 — the gate, end-to-end.** A branch fixture introducing the §1 worked
  example's leak makes `verify` BLOCK with exactly one new violation; reverting
  it returns STRUCTURALLY-CLEAR. The drift ratchet: flipping a SATISFIED
  function to VIOLATED surfaces as a *new* finding against the old base.
- **O5 — key stability (D-OB6).** Changing only a finding's `detail` prose
  between base and branch produces zero new findings.

**Effective — empirical, time-boxed after OB-3 lands:**

- **E1 — seeded-defect catch rate.** Mechanically seed the target bug class
  into loansvc (delete a release call on one branch; move a publish above its
  audit) across ~10 variants. *Keep threshold: the gate fires on 100% of
  seeded intraprocedural violations* — anything less means the walk has a
  soundness hole, which is a defect, not a tuning matter.
- **E2 — abstention rate.** Configure realistic rules on loansvc and measure
  the CANT-PROVE fraction. The Go-specific bet (§"why worth having" in the
  idea doc) is that idiomatic `defer` keeps abstention low. *Kill threshold:
  if a majority of real acquire sites abstain, the SATISFIED proof is hollow —
  revisit the escape analysis or the interprocedural question before
  promoting the feature.*
- **E3 — false-positive budget.** Track VIOLATED findings dismissed as
  infeasible-path. First confirmed case triggers the planned allow-list work;
  *sustained noise (rule producing more dismissed than accepted findings) →
  that rule is removed.* Trust in the gate is the asset; one noisy rule
  spends it for all.
- **E4 — adoption (the existing ROI gate, now measurable).** Within one
  quarter of landing: at least one rule configured on a real service by its
  owners, with a low steady-state UNMATCHED rate. Rules that exist only in
  fixtures mean the feature was speculative — documented outcome, not a
  silent shelf.

## 10. Honest limits (carried over, still true)

VIOLATED is existential modulo path feasibility — tune toward soundness and add
an allow-list (mirroring layering's) only when a real infeasible-path false
positive appears, not preemptively. No interprocedural, no concurrency, no
value semantics: the check proves the *shape* of a function's effect discipline
over all paths, nothing more. The ROI gate from the idea doc stands: if these
rules sit unconfigured on real services, the feature was speculative — the
fixture proves the machinery, adoption proves the value.
