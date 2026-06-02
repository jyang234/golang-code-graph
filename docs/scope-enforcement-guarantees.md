# System Scope, Enforcement & Guarantees

This sits above the component specs. They describe how each piece works; this describes what the *assembled* system is, how it's enforced in v1, what it guarantees, and — most importantly — what it does not. Read §7 before pitching it to anyone.

---

## 1. Two pipelines, and the gated-vs-generated line

Each pipeline produces a **gated** artifact (stable, consequential, routes to review) and a **generated, non-gated** view (richer, more volatile, for humans and AI):

| Pipeline | Gated artifact | Generated, non-gated view |
|---|---|---|
| Static | inter-service **boundary contract** + boundary blind-spot manifest | full call graph + signatures |
| Behavioral | per-flow **canonical snapshot** (tier-filtered) | rendered Mermaid sequence diagram |

The gated artifact in both is low-churn because each applies a *keep-only-the-consequential* projection — static keeps the boundary, behavioral keeps spans at or above the tier threshold. The generated views never gate, so the function-level churn from refactoring (static) and renderer drift (behavioral) never reach the gate.

---

## 2. "Contract" means two different things — keep them named

- **Behavioral observed contract (per-flow):** what the service *did* on a tested flow — DB operations, ordering, the events and calls it actually made. Deep, sampled.
- **Static inter-service boundary (whole-service):** what the service *can* publish, consume, call externally, and expose, across all statically-reachable paths. Shallow, exhaustive. **Excludes DB** — the database is the service's own store, owned by the behavioral side.

These are different surfaces with different scopes. Using "contract" loosely across them is exactly the kind of drift the consistency pass exists to catch; name them distinctly in specs and in conversation.

---

## 3. Complementary, not symmetric — and the coverage capability

- **Static** is shallow + exhaustive: every statically-reachable boundary effect, including error and rarely-hit paths no test exercises.
- **Behavioral** is deep + sampled: exactly what happened on the flows you wrote, with real ordering, DB operations, and values.

They cover each other's blind spots. The emergent, valuable capability: **the delta between the static boundary (all paths) and the union of behavioral snapshots (tested paths) is contract-level coverage feedback** — "you publish `disbursement.failed` on some path that no flow exercises." Neither pipeline produces this alone. It is a first-class output, not a side effect, and it's the mechanism by which the system surfaces *absence* (an untested path) rather than only *thinness*.

---

## 4. The tested-behavior contract

The behavioral artifact is a faithful function of the **tests**, not a proxy for production truth. Its promise is precisely "this is what your tests exercise." Three consequences:

1. **Feedback, by design.** A thin or unrealistic test yields a thin or unrealistic artifact, and that visible thinness is the signal to write a better test. The snapshot doubles as behavioral-shape feedback on test quality — coverage for *shape*, not percentage.
2. **The bound — hollowness, not falsity.** The mirror shows where a test is *hollow*, not where it is *wrong*. A mock returning a plausible-but-wrong shape produces a fine-looking, internally-consistent artifact that surfaces nothing. Wrong-assumption inadequacies need consumer-contract tests or real integration, not this.
3. **Test changes move the baseline.** Restructuring a test (swapping a mock, changing a stub's return) can change the golden with no production-code change. That's correct — the test defines the contract-as-tested — but reviewers should expect a test-only diff to be a legitimate baseline shift.

---

## 5. v1 enforcement model

- **Author-side manual regeneration.** Before opening the MR (or after fixing review feedback), the author runs regeneration, commits the updated artifacts alongside the code, and the artifact delta rides in the MR for the reviewer. This mirrors the existing pre-MR `/review` workflow.
- **CI staleness backstop (kept, near-free).** The in-process flows run in the normal `go test` suite and fail if a committed golden is stale; the boundary contract is regenerate-and-`git diff --exit-code`. This isn't the full gate ceremony — just "the checks already running also catch staleness" — and it's what preserves the always-accurate guarantee against a forgotten regeneration.
- **Per-MR behavioral surface = in-process, single-service flows.** Fast, deterministic, one clock domain. Out-of-process e2e is now available as a **separate, non-gated-by-default** path (`flowmap behavior ingest`; see `integration/otlp-integration-guide.md`), opt-in to a no-new-effects gate per flow; full inter-service E2E assembly remains deferred.
- **CODEOWNERS routes** the two gated artifacts (and the config and per-flow declarations) to humans. The human is the oracle; no AI sits in the verdict.
- **Reserved extensions:** inter-service E2E, and — as a cheaper intermediate that closes the downstream-drift gap without a full multi-service environment — consumer-driven contract tests (Pact-style).

---

## 6. What the system guarantees

- **Currency.** The committed artifacts match the code/tests (the staleness backstop), so the maps and snapshots are accurate rather than rotting documentation.
- **No silent statically-resolvable boundary change.** A new or removed published/consumed event, external dependency, or exposed entry point that the analysis can resolve changes the gated boundary contract and routes to a human.
- **No silent behavioral change on a tested flow.** A change in what a tested flow does changes the golden and routes to a human.
- **Human verdict.** The deterministic machinery only surfaces and routes; it never judges. No AI in the gate.
- **Disclosed uncertainty.** Every artifact flags where it's blind — the boundary blind-spot manifest, the `Complete` flag, truncation-fails-loudly.

---

## 7. What the system does NOT guarantee — read before pitching

- **Correctness.** It shows *change* and *coverage gaps*; it does not prove the code is right. The human judges.
- **Completeness over dynamic constructs.** A dynamically-constructed event or route on an untested path is invisible to both pipelines; the boundary blind-spot manifest flags that you're in that situation rather than hiding it.
- **Faithfulness beyond the test doubles.** The behavioral artifact is only as real as the test's DB and mocks — a real Postgres (testcontainers) is what makes DB operations trustworthy; with a fake, that portion is thin or fictional. And it shows hollowness, not falsity.
- **Coverage of untested branches.** Error and alternate paths exist in the snapshots only if you wrote flows for them; the §3 coverage delta tells you which boundary effects you haven't.
- **Value for pure orchestration services.** A service whose whole job is cross-service choreography is thin in-process (mostly calls-to-mocks); its real behavior lives in the deferred E2E / contract-test layer, not the per-MR gate.
- **Immunity to snapshot fatigue.** If reviewers rubber-stamp golden updates, the gate becomes theater. The prioritized semantic diff and the coarse tier-filtered snapshot mitigate this; review culture — and watching the regeneration rate as a leading indicator — is the residual control.
- **Zero adoption cost.** It rides existing OTel and integration-test patterns *where they exist*; where they don't, the behavioral half is net-new test authoring. Choose a lighthouse service that already tests well.
