# path obligations: implementation plan

**Status:** plan-of-record, refined from
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
same way the classify-hint matcher does). Functions with no relevant site
produce nothing — the common case, so cost is near zero on rule-free services.

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

**Abstention (`CANT-PROVE`) triggers — disclosed, never silent:**

- the acquired value escapes the function: returned, stored to a heap object,
  or passed to a first-party callee (release-in-callee is the interprocedural
  case slice 1 does not attempt);
- `recover` is present (control rejoins invisibly);
- the CFG is irreducible.

**Determinism.** Findings sorted by (rule, fn FQN, site position); positions are
`file.go:line` from the SSA instruction, stable across runs — same property the
rest of the pipeline already guarantees, so the artifact digest stays stable.

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
| `TransferEscape` | acquire, tx passed to helper | must-release `CANT-PROVE` |
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

## 9. Honest limits (carried over, still true)

VIOLATED is existential modulo path feasibility — tune toward soundness and add
an allow-list (mirroring layering's) only when a real infeasible-path false
positive appears, not preemptively. No interprocedural, no concurrency, no
value semantics: the check proves the *shape* of a function's effect discipline
over all paths, nothing more. The ROI gate from the idea doc stands: if these
rules sit unconfigured on real services, the feature was speculative — the
fixture proves the machinery, adoption proves the value.
