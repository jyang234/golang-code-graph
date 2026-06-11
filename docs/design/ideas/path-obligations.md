# Idea: path-obligation checks — augmenting the intraprocedural ("buy") leg

**Status:** exploratory idea, not built. This distills a design discussion; it is
a proposal to weigh, not a committed plan.

Related reading: the build-vs-buy and "enhance the graph vs build a different
one" arguments in [`../../groundwork/distilled-learnings.md`](../../groundwork/distilled-learnings.md);
the three-valued / honesty discipline in
[`../../groundwork/pressure-test.md`](../../groundwork/pressure-test.md).

---

## Where this fits: three legs of deterministic verification

Deterministic (no-AI, reproducible) code-quality verification has three legs, not
two:

| Leg | Verifies | Owner |
|---|---|---|
| **Inside a function** (CFG/AST/types) | logic, nil, bounds, unchecked errors, leaks | **buy** — compiler, `go vet`, `staticcheck`, `errcheck`, `-race`, `govulncheck`, `bodyclose`, `sqlclosecheck` |
| **Across functions, structural, all-paths** | layering, reachability, boundary surface, contract | **build** — groundwork (static graph) |
| **Across functions, behavioral, sampled** | real ordering + values on tested flows | **build** — flowmap behavioral (golden snapshots) |

"**Buy**" means *adopt a tool someone else already built*, not pay for one — in
Go these are free OSS. The real cost of buy is integration, noise-tuning (so the
check stays trusted), and CI time, never a license. We build only the slice no
adopted tool can know, because it depends on **our** domain semantics.

This idea is about the first leg. We do **not** want to rebuild it. We want a
narrow, high-value augmentation that the generic analyzers structurally cannot
provide.

## The enabling fact: the CFGs already exist

flowmap's static pipeline already runs `ssautil.Packages` (full SSA) en route to
the call graph, and every call-graph node holds a `*ssa.Function`. An
`ssa.Function` **is** a per-function control-flow graph (`.Blocks`, with `defer`
and dominance modeled). So flowmap already constructs the CFG for the entire
service and currently reads it only interprocedurally (who-calls-whom),
discarding the intraprocedural structure.

Augmenting the CFG leg here therefore needs **no new infrastructure** — the raw
artifact is already in memory. That is what makes this cheap *in this repo
specifically*.

## What NOT to do

Do not build a general intraprocedural linter. `staticcheck` / `vet` / `errcheck`
/ `-race` / `govulncheck` / `bodyclose` / `sqlclosecheck` / `rowserrcheck` own the
*generic* inside-a-function lane; re-deriving them is rebuilding solved work, and
a 95%-right general CFG would dilute the one property the system's credibility
rests on — exact where it claims, honest where it can't. The compiler and those
tools stay the leg.

## The primitive: path obligations

Almost every uncovered *intraprocedural-but-domain-specific* bug is one of two
shapes, and **both are dominator-tree queries** on `ssa.Function.Blocks`:

- **must-release** — after an *acquire*, every path to function exit must hit a
  *release* (tx commit/rollback, custom resource close, buffer flush).
- **must-precede** — every *B* site must be dominated by an *A* site
  (audit-write before publish; auth-check before a privileged call).

These are not generic resource checks; they are keyed to **our** named functions,
which is exactly why they fall in the *build* quadrant. `sqlclosecheck` knows
`*sql.Rows`; nothing off the shelf knows "our `eventbus` session must flush before
publish."

## Proposed architecture (keeps the trust boundary intact)

```
.flowmap.yaml        obligation rules (domain-declared, CODEOWNERS-gated)
   │                 acquire/release or require/before, by function FQN
   ▼
flowmap (has SSA) ── runs the dominance analysis ──► graph.json gains an
                                                      "obligations" section:
                                                      {rule, site fqn+line, status}
   │
   ▼
groundwork ──── consumes it like any other finding: VIOLATED → violation,
                CANT-PROVE → caution (require_proof can escalate); review/verify
                report only NEW obligation violations base↔branch
```

- **flowmap produces** (it is the only side with SSA, and it is the trusted CI
  side); **groundwork judges and contextualizes** — the existing split.
- The `obligations` array is a **separate, FQN-keyed disclosure section**, not
  folded into the call-graph edges. This is the doc's sanctioned form ("narrow
  level-2 slices keyed to FQNs, as a separate small artifact"), so it does not
  dilute the structural purity of the call graph.
- groundwork needs **near-zero new plumbing**: an obligation finding flows through
  the same fitness / review / verify machinery as a layering or reachability
  finding, including base-vs-branch "report only new violations".

### Worked example

Rule:

```yaml
obligations:
  - name: tx-must-close
    acquire: "example.com/svc/internal/store#BeginTx"
    release: ["example.com/svc/internal/store#Commit",
              "example.com/svc/internal/store#Rollback"]
```

Code:

```go
func (s *Store) Transfer(ctx context.Context, from, to string, amt int) error {
    tx, err := s.BeginTx(ctx)          // acquire
    if err != nil { return err }
    if err := debit(tx, from, amt); err != nil {
        return err                     // ⛔ leak: no release on this path
    }
    if err := credit(tx, to, amt); err != nil {
        tx.Rollback(); return err      // released
    }
    return tx.Commit()                 // released
}
```

The dominator walk finds the `debit`-failure `return` is reachable from the
acquire and is not post-dominated by any release (no `defer` covers it) →
`tx-must-close: VIOLATED at Transfer`. Nothing else catches this: a **trace** sees
it only if a test exercises the debit-failure path; the **call graph** has no
ordering/branching; **generic linters** don't know `BeginTx` is a resource.

## Determinism and honesty (non-negotiable)

- SSA construction and dominator analysis are both deterministic → stable findings
  → stable digest.
- Three-valued, like everything else: `SATISFIED` / `VIOLATED` / `CANT-PROVE`. The
  abstention fires when the obligation crosses a function boundary (acquire here,
  release in a callee — the interprocedural case we do not attempt initially), the
  CFG is irreducible, or control can leave invisibly (`panic`/`recover`). A
  `CANT-PROVE` is a disclosed blind spot, never a silent pass — same discipline as
  `<dynamic>`.

## What these checks prove (and what they don't)

The core proof is an **all-paths control-flow obligation**: "on *every* path this
function can take, X happens, in this order." The two verdicts have asymmetric
strength:

- **SATISFIED is the valuable proof — a universal.** "No modeled path leaks": an
  absence-over-all-paths guarantee, the class tests cannot produce. Sound up to
  CFG completeness (the abstention cases above).
- **VIOLATED is weaker — an existential, modulo feasibility.** "A path exists in
  the CFG where acquire happens and release doesn't." That path is *syntactically*
  present; a path-insensitive analysis can't prove it is *feasible* at runtime.
  Usually the same thing for error paths; not guaranteed.
- **CANT-PROVE proves nothing** — and says so.

It explicitly does **not** prove: path feasibility; that the release was the
*correct* choice (it checks the *shape* of the lifecycle, not the semantics — you
can satisfy "tx closes on every path" while committing a logically half-done
transaction); values/data (same mode-2 wall — "audit precedes publish", not "the
audit content is right"); interprocedural or concurrent properties.

In one line: it proves **the shape of a function's effect discipline is complete
over all paths** — a required effect happens, in order, on every branch — not that
the function's logic or values are correct.

## Why it is worth having

1. **Closes the all-paths-lifecycle / sin-of-omission residual** — the gap nothing
   else in the stack covers. "The agent forgot the rollback on the error branch"
   moves from "hope a test hits that branch" to "proven over all branches, or
   disclosed."
2. **An oracle independent of the agent** — like groundwork's reachability and
   unlike the behavioral pipeline (which runs agent-authored tests), it cannot
   share the agent's blind spots.
3. **A drift ratchet at branch granularity** — asserted once, no future change can
   drop the cleanup/audit on some path undetected.
4. **Targets a high-cost, poorly-tested bug class** — leaks, missing audit,
   tx-on-error: common, combinatorially hard to test exhaustively, expensive in
   production.

A Go-specific reason it is more than theoretical: idiomatic Go puts cleanup in
`defer` *in the same function*, so the common lifecycle case **is**
intraprocedural — the abstention rate stays low and the SATISFIED proof actually
covers the bulk of real code rather than abstaining everywhere.

## The build/buy line stays sharp

- **Buy:** `go vet`'s `lostcancel` (ctx cancel), `bodyclose`, `sqlclosecheck`,
  `rowserrcheck` — the generic resource lifecycles. Also the salience-directed use
  of *existing* analyzers (run them, then rank/enrich findings with the graph's
  tier + blast radius) is "buy + contextualize", not "build".
- **Build:** only the obligations expressed over *our* named functions — the ones
  no off-the-shelf tool can know.

## Suggested first vertical slice (if prototyped)

The smallest end-to-end cut that proves the idea:

1. One obligation kind — **must-release, intraprocedural, `defer`-aware**.
2. A new fixture with a leaking error path (as we added `layeredsvc` / `blindsvc`).
3. flowmap emits an `obligations` section in `graph.json` for that rule.
4. groundwork's `fitness` reads it (`VIOLATED` → violation, `CANT-PROVE` →
   caution); `review` / `verify` get base-vs-branch new-obligation reporting for
   free.

One obligation, one dominance query, one fixture, ~zero new groundwork plumbing —
and it closes the all-paths-lifecycle residual for the domain-specific case.

## Open questions

- **Interprocedural obligations** (acquire in one function, release in a callee):
  abstain initially, or attempt a bounded interprocedural extension? The latter
  reintroduces the determinism/blind-spot surface the call graph was careful to
  bound.
- **Feasibility / false positives on VIOLATED**: tune toward soundness (report the
  syntactic path) and rely on allow-lists for known-infeasible cases, mirroring the
  layering allow-list?
- **Rule home**: `.flowmap.yaml` (flowmap evaluates, so the rules must reach it)
  vs. a shared policy surface with groundwork. Keeping evaluation where SSA lives
  argues for `.flowmap.yaml`.
- **ROI gate**: like mode-2 value-flow, build only if these bugs are actually
  reaching production past the bought analyzers and the e2e suite — not on spec.
