# Three seats, before and after — how Claude Code's behavior changes

> **`ACTIVE`** · narrative guide (before/after) · _reviewed 2026-06-18_

The fastest way to understand this toolset is not its feature list but its
effect on the three people (and their agent) who touch a service: the
**incident responder**, the **feature developer**, and the **PR reviewer**.
For each, the contrast that matters is *Claude Code alone* versus *Claude Code
holding these instruments* — because the tool does not make the model
smarter. It changes three things: **what kind of claims the session contains**
(checkable facts instead of plausible prose), **where the model's attention
goes** (a computed map instead of unbounded search), and **whose judgment the
verdicts rest on** (a deterministic judge outside the author, instead of the
author's own reading).

Everything below maps to runnable commands and committed tests; the measured
numbers come from [`drills.md`](drills.md), and the unproven edges are graded
honestly in [`scorecard.md`](scorecard.md).

---

## Seat 1: the incident responder

**The scenario.** A Dynatrace problem opens: failure-rate spike on
`POST /api/v1/loan-application`, a degraded downstream (`credit-bureau`), a
stack trace in the details. The responder opens Claude Code and pastes the
alert.

**Before (Claude Code alone).** Claude greps for route strings, reads
handlers, follows calls by hand, and builds a hypothesis from pattern-matching
— consuming its context window on reconstruction, with recall that degrades
silently as the codebase grows. Its claims ("this handler calls the bureau
client") *sound* identical whether verified or guessed. The most valuable
incident question — *what has already irreversibly happened if this call
faulted?* — is answerable only by hopeful code-reading, and the worst failure
mode is invisible: investigating yesterday's code while today's is deployed.

**After.** The alert's own words are tool inputs: `triage --route`,
`--frame` (runtime trace form pasted verbatim), `--table`, `--event`,
`--peer`. One call returns the bounded suspect set, the implicated
entrypoints, the effect surface, and the blind spots where the map stops being
sound — measured at **10/10 recall with a median hunt space of 8% of the
graph** (routes: 3%). The fault card answers the irreversibility question
deterministically: *"`loan.approved` was CERTAINLY committed before the fault
site"* — read off CFG dominance, not log spelunking. `--expect $DEPLOYED_SHA`
makes the stale-map failure loud instead of silent, and the incident's own
OTel trace (`flowmap behavior ingest`) locates the divergence *inside* the
suspect set the graph bounded. Claude's behavior change: from unbounded
explorer to interpreter of an instrument panel — and every claim it relays is
re-derivable by the human beside it.

**What doesn't change:** causes outside the code (config, infra, data) are
not on this map — every fault card says so, in those words.

---

## Seat 2: the feature developer

**The scenario.** Build a refund flow: new route, a payment-gateway reversal,
ledger compensation, a `loan.refunded` publish. Complex enough to touch
money, ordering, transactions, and the inter-service contract.

**Before (Claude Code alone).** Claude plans from grep-and-read impressions
of the architecture. The team's rules live in `CLAUDE.md` — advisory prose
the model can misread, deprioritize under context pressure, or never see.
Constraints arrive as *rejections*: a human reviewer (days later) notices the
handler skipped the app layer, the rollback is missing on one error branch,
the publish happens before the charge. Each rejection costs a round trip, and
the same class of mistake recurs because nothing ratchets.

**After — the loop becomes ground → edit → verify, one rule set at both
ends.**

- **Planning:** Claude interrogates the real structure (`reach`, exploratory
  `triage`) and — decisively — `ground(fqn)` on every function the plan
  touches. The card lists what *binds* an edit there, derived with the same
  matchers the merge gate uses: the auth waypoint, the tx lifecycle
  obligation, the write budget, and the existing partial-effect exposure
  (disburse publishes before its fallible charge — so refunds should order
  reversal *first*). Constraints become requirements before generation, and
  the plan includes its own **rule changes**: a new must-precede obligation,
  CODEOWNERS-reviewed like code, encoding the lesson at the feature's birth.
  Where a committed behavioral corpus exists, the audit-only `impeach` lens adds
  the one signal static analysis structurally cannot give: *where observed
  behavior has already contradicted the graph's "this can't happen"* — so Claude
  is warned off trusting a proven-absent claim at a seam the runtime has shown is
  real, before it edits there. (It only ever discloses — the gate is `verify`.)
- **Implementing:** Claude self-checks deterministically before pushing —
  regenerate the graph locally, run `verify`. Every classic agent mistake
  fails with a witness: the layering skip names the bypass path, the missing
  rollback names the leaking exit by line, the premature publish fires the
  new obligation, "make it async" trips `no_concurrent_reach`, reaching for
  reflection trips the blind-spot ratchet. Iteration happens against a judge
  that never tires and consumes zero human attention.

The behavior change in one line: **deterministic prevention replaces
deterministic rejection** — the same gate that would have bounced the PR is
consulted before the code exists.

**What doesn't change:** whether the refund math is right, the API shape is
wise, or the feature should exist. Value correctness and judgment stay with
the engineer and the tests.

---

## Seat 3: the PR reviewer

**The scenario.** The refund PR arrives: 40 files, authored largely by an
agent, description says "no behavioral changes."

**Before (Claude Code alone as review assistant).** Claude reads the diff and
reconstructs structure — the same expensive, size-degraded work as seat 1,
duplicated. Its review claims are uncheckable ("doesn't appear to break
layering"), it can't distinguish violations *this PR introduced* from
five-year-old debt, two runs produce two different comment sets, and — the
unsolvable part — when the author was also Claude, the reviewer shares the
author's training, prompts, and blind spots. A correlated reviewer is a weak
oracle, at any capability level.

**After.** The computed artifact arrives first: a three-valued verdict
(BLOCK / STRUCTURALLY-CLEAR / NO-STRUCTURAL-SIGNAL), the shape, *additive vs
breaking* contract changes, new I/O effects, blast surface, and only the
**newly-introduced** findings — diffed by canonical identity against base.
Claude's review becomes interpretation of an independent instrument: its
sentences turn into citations anyone can re-derive (`verify-artifact` catches
even a re-signed forgery), its variance is confined to prose and judgment,
and the verdict allocates its attention mechanically — NO-STRUCTURAL-SIGNAL
means *all* context goes to logic, "exactly where logic review matters most."
The independence is architectural, not behavioral: the judge shares nothing
with the author — not the model, not the prompt, not the failure modes. And when
CI runs the gate with `--corpus`, the artifact can carry the strongest verdict it
has — a *behaviorally-confirmed* `VIOLATED`: a `must_not_reach` breach the runtime
itself witnessed, not a heuristic, with the observed trace as the evidence.

**What doesn't change:** logic, values, taste, and any invariant nobody
declared. The artifact is only as honest as the policy — which is why dead
rules and dead exceptions disclose themselves.

---

## The five shifts, across all three seats

1. **Bounded inputs replace unbounded search** — cards in hundreds of tokens
   instead of reconstruction across thousands; quality independent of
   codebase size (measured on the fixture; scale honestly unproven).
2. **Checkable claims replace plausible prose** — the session's load-bearing
   statements become re-derivable by a third party, which is what lets a
   human actually delegate weight to an agent's output.
3. **Prevention precedes generation** — the gate's rules are consulted before
   the edit, not after the PR.
4. **The judge is outside the author** — the property no agent can have about
   itself, by construction; it matters most exactly when author and reviewer
   are the same model.
5. **Everything ratchets** — findings diff against base, drills are test
   assertions, exceptions are audited: improvement is locked in, regression
   is loud.

And the standing honesty, in every seat: the model's judgment is unchanged;
the instruments only report what they are plumbed to sense; the behavioral
signals (the `impeach` lens, the corpus gate) are dormant until a service has
committed a golden corpus, and even then they find counterexamples only on
*exercised* paths — never a proof that the graph is right; and the claim that an
agent *performs measurably better* with these tools is the one experiment (E4)
that requires live sessions — designed, criteria named, results slot waiting.
