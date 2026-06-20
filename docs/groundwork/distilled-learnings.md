# groundwork: distilled learnings

> **`DESIGN RECORD`** · the thesis & distilled learnings · _reviewed 2026-06-13_

A concise record of what we established about using the event-bus call graph as
a grounding and verification substrate for AI-accelerated engineering.

---

## The core thesis

An LLM editing code reasons **locally** (the function in front of it).
Correctness is a **global** property (what's reachable, what breaks, what
contract shifts). The gap between those is where agentic code generation fails
silently. A call graph with boundary semantics and tier labels is the cheapest
available proxy for that global structure — it doesn't make the model smarter,
it closes the loop the model can't see.

---

## What the graph deterministically IS (and isn't)

The file is a static call graph: nodes (`fqn`, `sig`, `tier`, `package`, `fallible`),
edges (caller→callee, with `concurrent` and `boundary` flags), and blind spots.

**Exactly recoverable** (not heuristics — they fall out of the data):
- Full function inventory + signatures; full static call structure.
- Reachability both directions → blast radius, dead-code candidates, route cover.
- Error skeleton: 510/872 functions are `fallible`; trace which error paths reach entrypoints.
- Concurrency boundaries: 64 `concurrent` edges mark goroutine spawns.
- **Typed external I/O surface:** 37 boundary edges — 35 `outbound-sync` (DB, with named ops like `INSERT subscribers`), 1 `outbound-async` (bus PUBLISH), 1 `inbound` (bus CONSUME).

**Deterministically NOT in it:** ordering/branching inside a function, data/values
(why the two bus edges are `<dynamic>`), anything resolved at runtime, and any
semantic notion of "correct."

**The sharpest property:** the graph is exact about structure *and explicit about
where structure runs out*. The 107 blind spots aren't gaps — they're the file's
own statement of its epistemic boundary. That honesty is rarer and more valuable
than the structural map itself, because anything built on it can know when to
abstain rather than confidently hallucinate.

---

## Where control flow stops being the same object (and why we stopped)

There's a hierarchy, each level categorically bigger:
1. **Call graph** (this) — who calls whom. Interprocedural, *composes*, small, exact.
2. **Control-flow graph** — order/branching within a function. Per-function, *doesn't compose*, 10–30× larger.
3. **Data/value flow** — which values flow, which branch fires. Path-sensitive, undecidable in general; where the `<dynamic>` walls live.

Decision: **keep level 1 as the substrate.** Folding in general control flow
dilutes the one property that makes the graph good for verification — exact about
structure, honest about limits. A partial CFG loses that honesty (95%-right branch
analysis looks identical to the 5% it's wrong about). If specific ordering bugs
matter, extract *narrow* level-2 slices (e.g. defer/cleanup-path, error-handling-path)
as separate small artifacts keyed to these FQNs — not one giant approximate CFG.

---

## What we built: `groundwork`

One engine, three surfaces (committed artifact, CLI/CI, MCP server). Capabilities:

- **`impact`** — blast radius: transitive callers (who breaks), callees (what it depends on), entrypoint cover, blind spots in path, explainable risk score.
- **`ground`** — per-function grounding card for an agent to read *before* editing; one-hop neighbors + a `trust: reliable|verify` flag that fires where the graph is unreliable.
- **`fitness`** — architectural invariants as deterministic pass/fail (below).
- **`verify`** — pre-flight gate: apply a proposed change *delta* to the graph, re-run all checks before merge, report only newly-introduced violations + scope escapes + a reproducibility digest.
- **`diff`** — boundary-contract diff; flags breaking external-surface changes.
- **`review`** — deterministic MR review artifact computed from base-vs-branch graphs (verdict, shape, new violations, contract movement, I/O effects, reach) + an unfakeable digest.
- **`verify-artifact`** — recompute an MR artifact's digest to prove it is authentic (not tampered, not stale).

The unifying primitive is the **fitness function**: a deterministic assertion
about graph shape that fails closed in CI. It converts an architectural fact from
"held in someone's head" to "re-checked on every change."

---

## Empirical findings from the real graph (not illustrative)

- **The architecture isn't what the names suggest.** The real request spine is `handler → app → storage` (92 direct `handler→app` edges); `api` is a *side* generated routing/serialization layer, not a middle tier. We only learned this by measuring the baseline before asserting an invariant.
- **Mild layer-bleed exists:** `app` (domain) calls `api`'s parameter-error types' `Error()` methods. A real allow-list-or-refactor decision the fitness function surfaced deterministically.
- **An isolation invariant already holds:** no event-delivery path reaches `publishWithFanout` (verified, 98 callers examined).
- **`publishWithFanout` has 98 transitive callers but 3 downstream deps**, one being `boundary:bus PUBLISH <dynamic>` — the service's entire reason to exist (routing events) runs through an edge the graph explicitly cannot resolve statically. **Read the 98 as an upper bound, not a precise count:** the backward reach fans out through the oapi-codegen `strictHandler` HighFanOut dispatch (RTA pairs every caller with every operation), so the cover is over-approximated. The cards now mark exactly this with `≤ (over-approx via dispatch)` whenever a cover crosses a HighFanOut seam.
- **Dead-code reality:** naive orphan detection flags 117; after suppressing closures + blind-spot sites the graph genuinely can't resolve, **14 real candidates** remain — a minutes-long triage.
- **I/O budget:** on a storage layer that builds SQL with non-constant strings, the per-route write count can read **0** because every CRUD write is labeled `db call` (the verb is unreadable), not `db INSERT/UPDATE/DELETE` — so the budget bounds nothing and a green is not a proof the write surface is bounded. The check now emits a caution (`write budget unenforceable on N route(s)…`) wherever this happens, and `review` reports a base→branch rise in the unclassified-DB fraction as fidelity drift. An earlier reading of "6 writes/route across 68 entrypoints" depended on the storage layer emitting compile-time-constant SQL the labeler could read.

---

## Engineering lessons (only visible by running on real data)

1. **Blind-spot awareness is mandatory, not optional.** A check that's silently wrong 5% of the time gets muted and abandoned. Suppressing exactly the unresolvable cases (closures, dynamic dispatch) is what keeps a check trusted. The graph's self-honesty is the feature that makes everything else credible.
2. **Measure the baseline before asserting an invariant.** Our first layering model was wrong about the actual architecture; the graph corrected it. You *derive* invariants from the graph, then enforce them.
3. **Allow-lists are required for usability.** Real layered code has legitimate exceptions (everything calls `Error()`). Review once, suppress forever, fail only on *new* violations.
4. **Risk must be local, not inherited.** First risk model gave a leaf the same score as a hub (both sit under the same fan-in). Fix: weight a function's *own downstream* blind spots heavily, upstream reach lightly.
5. **Scope prediction needs transitive reach, not one hop.** One-hop prediction produced false positives; bounded transitive reach calibrated it.
6. **Determinism is the whole point.** The verify verdict is a pure function of (graph, delta, policy) — identical digest across repeated runs. That's what an LLM-judge can't offer and what lets this be a hard CI gate the agent can trust and converge against.

---

## How it helps an agent — concretely

- **Plan:** predicted footprint to scope the work + grounding cards flagging blind spots, so the plan doesn't guess through the unreliable regions.
- **Deliver:** deterministic pre-merge gate that blocks new layering/reachability/budget violations, flags scope creep, catches breaking contract changes — each naming the exact symbol or edge.
- **Review:** a computed, unfakeable MR artifact that removes the comprehension tax of agent-authored change — the reviewer's attention lands on intent and sins of omission, not on reconstructing what moved.
- **Consistency across agents and time:** the policy file is the single source of architectural truth; every agent/PR/CI run is held to the same invariants, so quality doesn't depend on which agent ran or how it was prompted.

---

## What this is NOT verifying — the failure modes (mapped concretely)

A green `verify`/`review` certifies exactly one proposition: *the change did not
alter the declared structural shape of the system.* That is narrow and
load-bearing. The danger is not the gaps — it is mistaking the green check for a
broader claim than it makes; a check that displaces the scrutiny that would have
caught a bug makes you worse off than no check. The specific classes a passing
structural verdict leaves open:

1. **Logic wrong within an unchanged shape.** Off-by-one, inverted conditional, wrong constant. Anchored in real code: the three `delivery.clamp*` functions are structurally identical (same `int→int32` sig, same caller); change `clampReceiveBatch` to clamp to 100 instead of SQS's limit of 10 and every check passes, because the bound is a literal inside the body — not a node or edge.
2. **Data/value correctness.** `boundary:bus PUBLISH <dynamic>` — the graph knows a publish *happens* and *where*, not *what topic/payload*. Publishing the right event to the wrong topic passes; the service's whole purpose runs through the two edges the graph can't resolve.
3. **Over-approximated regions → false negatives.** A `must_not_reach` "pass" through a HighFanOut site means "the static graph found no path," not "no path exists." Sound only *outside* the blind spots; the `trust: verify` flag marks where to distrust it.
4. **Off-graph entirely.** Concurrency *correctness* (graph flags the 64 goroutine boundaries but can't see a data race), resource lifecycle (commit/close/cancel on every path — intra-function control flow), performance (an N+1 is the same edge whether it runs once or 10⁴ times), error *handling quality* (knows a fn is `fallible`, not whether the handler swallows it).
5. **Sins of omission.** A validation/audit-write/idempotency check the agent *should* have added leaves no edge to flag. You cannot verify the absence of a thing that was never there.
6. **The tool itself can be stale or wrong.** The verdict is only as good as the last flowmap run; the blind spots are flowmap *admitting* this. A pass against a stale graph is "consistent with a possibly-wrong map" — which is why regeneration discipline matters as much as the checks.

## Enhance the graph vs. build a different one

The dividing question: *does catching this need a new fact about the same nodes,
or a new kind of node entirely?* The call graph extends gracefully **outward and
along** (more edge semantics, value-flow, boundary constraints) and painfully
**inward and down** (control flow, statement order, path conditions).

| Failure mode | What's missing | Where it lives | Verdict |
|---|---|---|---|
| **Logic (mode 1)** | a literal / branch / operator | *inside* a function body | **Different graph** (CFG/AST) — except bugs tied to an external constraint, catchable by **enhancing the boundary model** |
| **Data/value (mode 2)** | the value on an edge | *between* functions, on existing edges | **Enhancement** — value-flow / taint along the call graph; the `<dynamic>` markers are typed slots to fill |

Mode 2 rides edges you already have: `publishWithFanout` receives the topic ARN
as a parameter that traces back to `awsnaming.TopicForVersion`/`TopicForTemplate`
— functions *in the graph*. So `<dynamic>` is resolvable to a named topic by
provenance, which would let publisher-output and subscriber-input sets be
compared (the core routing-correctness property). This *shrinks* the blind-spot
set rather than adding a parallel artifact.

## Build vs. buy — and what changes under 100%-agent development

For the QC goal, the build-vs-buy line falls in a specific place:

- **Intraprocedural correctness (mode 1)** → **buy.** Go's `staticcheck`, `go vet`, the race detector, `errcheck`, CFG-based linters already are a deterministic CFG and catch this lane well. Building your own CFG to re-derive them is rebuilding a solved problem. *CFG is worth having, not worth you building.*
- **Interprocedural value-flow (mode 2)** → **build, as enhancement.** No off-the-shelf tool knows your service's event-routing semantics. There's no linter for "the agent broke event routing."

**When the agent does 100% of the build (Claude Code + linters + e2e), the value
narrows sharply** — the only value-add is the *residual* the agent's own
self-correcting loop can't catch:

- The loop already closes mode-1 logic (linters), tested behavior (e2e), and most local mistakes (regenerate-and-retry). CFG is now *triple*-covered — dead. The value-flow enhancement drops in priority because good e2e covers most routing bugs at runtime.
- **The structural problem that gets *worse* with more autonomy:** when the agent writes the code *and* the tests, the tests encode the agent's own understanding — they share its blind spots exactly. A misread spec yields a confidently-passing test asserting the wrong behavior. **The agent grades its own homework.** The more autonomous the agent, the more you need an oracle *independent of the agent's judgment*.
- That isolates value-add to the small set that is **both independent of the agent AND uncatchable by its own tests:**
  1. **The anti-drift ratchet.** The real failure at scale isn't one wrong line — it's *drift*: across hundreds of changes the architecture erodes, no single diff wrong enough to fail review, and agent-authored tests drift *along with* the code. A fitness function asserted once is a ratchet no session can loosen.
  2. **All-paths safety/negative invariants.** "X is never reachable from an unauthenticated route" — a test shows it didn't happen in enumerated cases; reachability *proves* it over all paths. Categorically beyond any test suite, and exactly the fintech-stakes class.

The settled verdict: **don't build CFG/AST for QC. Adopt the existing analyzers
for the inward-and-down lane; spend build effort on the structural drift-ratchet
and a few all-paths safety invariants — the unique quadrant.**

## Deterministic MR artifacts — the reviewer comprehension problem

In 100%-agent development the bottleneck moves to the human reviewer, who has
lost the passive comprehension of having written the code. The trap: the MR
description is authored by the system under review, so it can be confidently
wrong or paper over a problem. **You cannot fix an AI-comprehension problem with
more AI-generated prose.**

The fix: a review artifact computed *from* the base-vs-branch graphs, not
*about* them — verdict (BLOCK/CLEAR), shape (body-only/localized/cross-package/
broad), new invariant violations with the exact edge, contract movement, I/O
effects, and which entrypoints the change is now live behind. The reviewer spends
~15s confirming "structurally this is what it claims," then spends real attention
on intent — the part only a human can judge.

**The unfakeable guarantee — corrected by pressure-testing.** Each artifact
carries `digest = sha256(structural content)`, but the digest is **not a
signature**: an agent can recompute it over a doctored body. The security lives
in the **verifier re-deriving the artifact from the source graphs** — and that in
turn rests on one load-bearing condition surfaced by attack: **the graph must be
generated by trusted CI from the branch code, never supplied by the agent.** An
agent that generates its own branch graph forges a pass trivially by omitting the
offending edge. flowmap execution must sit inside the CI trust boundary.

**The verdict is three-valued, also from pressure-testing.** A body-only change
first rendered `CLEAR` — the same green as a verified-safe structural change —
which a hurried reviewer would misread as "logic reviewed," skipping the review
that change most needs. Fixed: `BLOCK` (invariant/contract broke),
`STRUCTURALLY-CLEAR` (shape preserved, *not* a logic sign-off), and
`NO-STRUCTURAL-SIGNAL` (body-only; the graph abstains and says so). The tool's
silence is now legible instead of disguised as approval.

Demonstrated on the real graph: a feature MR labeled "add publisher health
endpoint" that also quietly wires the handler straight to storage renders
**BLOCK** naming the skip-level edge; the same feature done correctly renders
**CLEAR**. Same description, different *computed* verdict — exactly the
comprehension the reviewer was losing.

**Engineering lessons added here:**
7. **The agent's own tests aren't an independent oracle** — they share its blind spots. Autonomy raises the need for agent-independent checks (structural invariants), not lowers it.
8. **Drift, not single bugs, is the scale failure** — and the one thing a self-correcting agent loop + agent-authored tests structurally cannot self-enforce. The asserted-once fitness ratchet is its specific antidote.
9. **The graph extends outward-and-along cheaply, inward-and-down painfully** — let the toolchain own the internals; spend your effort where you're the only one who can (cross-function, domain-specific, all-paths).
10. **Explanations must be computed, not authored** — an artifact derived from the code is unfakeable in a way agent-written prose never is; verify it by recomputation, distinguishing tampered (body edited) from stale (wrong code).

---

Everything here verifies **structure** — dependencies, reachability, side-effect
surface, contract. It guarantees an agent cannot *silently* erode the
architecture, breach an isolation boundary, balloon a route's side effects, or
break the service contract. It does **not** verify the logic inside a function is
correct — that stays tests and types. Fitness functions are the cross-cutting,
global layer those tools don't cover, enforced *before* merge rather than
discovered after.

**The single point of failure (from pressure-testing):** every guarantee above
is conditional on the **integrity of the graph**, which must be generated by
trusted CI from the branch code. An agent permitted to supply its own graph
forges any verdict by omitting edges — the trust boundary is around flowmap
execution, not around groundwork. The "unfakeable digest" was overstated: the
digest catches accidental edits and staleness, but unforgeability comes from a
trusted party *recomputing* the artifact from the source graphs, not from the
number itself. See [`pressure-test.md`](pressure-test.md) for the full
adversarial analysis.

---

## Open next step

Both `verify` (pre-flight delta gate) and `review` (base-vs-branch MR artifact)
are built and validated against the real graph. The one remaining integration is
**automating the graph diff**: run flowmap on the agent's branch and on base, and
feed both to `review`/`verify` so they run untouched on every MR with zero manual
input — no hand-written delta. Everything downstream of that (the artifact, the
digest, the gate) already works; this is the plumbing that makes it
zero-touch in CI.

Lower-priority, shelved deliberately: the mode-2 value-flow enhancement
(resolving `<dynamic>` topics via `awsnaming.*` provenance). High-value in
principle, but under good e2e its marginal value is small — revisit only if
routing bugs slip past tests in practice.
