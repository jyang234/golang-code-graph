# groundwork

`groundwork` is a deterministic consumer of flowmap's static call graph: it turns
the "what can happen" map into computed verification artifacts — architectural
fitness functions, blast-radius/reach, fail-closed pre-merge gates, and unfakeable
MR review artifacts. No AI sits in any verdict; every output is a pure function of
`(policy, graph, delta)`, inheriting flowmap's human-as-oracle model.

**Status:** the engine and the full surface are built and tested — the verdict
surfaces `fitness`, `review`, `verify`, `diff`, `verify-artifact`,
`policy-check`; the lenses `reach`, `triage`, `ground`, `exceptions`,
`transcript`; the adoption shell `init` (+ SARIF, setup action); and the `mcp`
server through all three tiers (stdio, `--service` fleet serving, `--http`
team transport). The zero-touch CI trust anchor (Phase 4) is intentionally
deferred; until it exists groundwork is a sound *local/advisory* tool, not yet
an adversary-resistant gate (see the trust boundary in the usage guide).

## Start here

- **[`personas.md`](personas.md)** — the three seats (incident responder,
  feature developer, PR reviewer), each contrasted before/after with Claude
  Code: what kind of claims the session contains, where attention goes, and
  whose judgment the verdicts rest on.
- **[`drills.md`](drills.md)** — the measured effectiveness record: the staged
  incident drills (recall, scoping power, the trace handoff, the staleness
  demonstration) committed as test assertions, plus the E4 design awaiting a
  live run.
- **[`scorecard.md`](scorecard.md)** — the capability scorecard, graded by
  evidence class: what is proven, what is measured, and what is honestly
  unproven from inside this repo.
- **[`usage.md`](usage.md)** — the practical guide: how groundwork and flowmap fit
  together, every command with real examples, a worked end-to-end review, and the
  trust boundary. Read this first to *use* it.

## The design record (the *why*)

1. [`distilled-learnings.md`](distilled-learnings.md) — the core thesis, what the
   graph deterministically is and isn't, the failure modes a structural verdict
   leaves open, build-vs-buy, and the engineering lessons.
2. [`mr-review-artifacts.md`](mr-review-artifacts.md) — the deterministic
   base-vs-branch review artifact and its anti-fake guarantee (one surface, in
   depth).
3. [`pressure-test.md`](pressure-test.md) — the adversarial analysis that
   corrected "unfakeable digest," surfaced the trusted-graph trust anchor, and
   made the verdict three-valued.
4. [`implementation-plan.md`](implementation-plan.md) — the plan-of-record:
   verified interface, package layout, phased build order with current status, and
   the corrections from the plan's own pressure test.

## Relationship to flowmap

flowmap **produces** the graph (`flowmap graph <dir>`) and the gated boundary
contract (`flowmap boundary <dir>`). groundwork **consumes** those JSON files and
judges them. They are deliberately separate programs so CI can control which runs
where — the security boundary is around flowmap execution, not around groundwork.
