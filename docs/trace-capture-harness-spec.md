# Trace Capture & Correlation Harness — Specification

The harness produces canonicalization's input: **a complete, scoped trace for exactly one flow.** Everything downstream (canonicalize → snapshot → diff → gate) assumes that input is correct and complete. Get this wrong and you either snapshot a truncated trace (a silent false golden) or include another test's spans (a noisy one).

A flow runs as: **arrange** (fix data, set up capture, mint a correlation key) → **trigger** (drive the entry) → **await** (wait for completeness) → **collect & scope** (gather exactly this flow's spans, reconstruct the tree) → handoff to canonicalization.

---

## 1. Capture mode follows test process topology

This is the sharpening from Q4. The capture mode isn't a free choice — it's determined by whether the test runs the system under test (SUT) **in its own process**:

| Test style | Capture | Why |
|---|---|---|
| Go in-process integration test (test wires the service and drives it through its real router / consumer path) | **in-memory exporter** | shared process; spans land in memory; scoping is free |
| Playwright (or any out-of-process) E2E against a running service | **post-hoc collector fetch** | SUT is a separate process — no shared in-memory exporter possible |

Since Playwright drives your backend services over HTTP, those tests are **out-of-process → post-hoc**. Any Go-level integration tests you keep are **in-process → in-memory**. Both are first-class; spec them both.

**Orthogonality worth being precise about:** capture mode (in-process vs post-hoc) is a *topology* question; the §3.3 hard ordering problem (cross-service clock skew) is a separate *service-count* question. A Playwright test against a **single** service is post-hoc but still single-clock-domain — the service's spans, including its DB-client spans, share one clock, so ordering stays easy. §3.3's hard path bites only when one flow fans across **multiple** services, regardless of capture mode.

- **In-memory mechanism:** OTel Go's `sdk/trace/tracetest` in-memory exporter + span recorder; read finished spans after quiescence.
- **Post-hoc mechanism:** SUT exports through an OTel Collector (with a test pipeline forced to `AlwaysSample`) to a trace store; fetch the trace by its trace ID. The store adapter (Jaeger / Tempo / OTLP backend) is a config point, not hardcoded.

---

## 2. Trigger shapes

Two entry types, matching the bus-as-boundary model (a flow begins at an inbound request **or** a consumed event).

**HTTP (Playwright).** Playwright's request API issues the call. The harness injects a `traceparent` header carrying a controlled trace ID with the sampled flag set (`...-01`), plus a `baggage` member with the test-run ID. Playwright is the client and produces no server spans — the captured trace is the SUT's server-side handling of the request.

**Event injection (consumer-rooted).** Publish a synthetic event to the bus with trace context injected into the message headers (standard OTel messaging propagation) and the test-run ID in baggage. The SUT's consumer span becomes the flow root. Prefer publishing through the **real bus** (exercises the true consume path) for E2E; direct handler invocation is the faster, lower-fidelity option for in-process tests.

---

## 3. Correlation — scoping to exactly this flow

Two keys, used together:

- **Controlled trace ID** (primary, precise). The test mints the trace ID and injects it via `traceparent`. If the SUT extracts incoming context (standard OTel propagation, which must be configured), it continues that trace, and the trace ID is the scope key.
- **Correlation attribute** (robust backstop). A unique `test.run.id` carried as OTel **baggage**, which propagates across async hops and the bus — surviving cases where server instrumentation starts a *new* trace on entry and merely links rather than continues. Baggage is not automatically a span attribute, so a small test-env span processor copies `test.run.id` from baggage onto each span, making spans queryable by it.

**In-process gets scoping for free** — the in-memory exporter only ever holds this run's spans, so correlation is a post-hoc concern. Post-hoc fetches by trace ID and filters by `test.run.id`.

---

## 4. Quiescence & completeness — the make-or-break

Synchronous flows hand you the output in the same call; async flows (event-driven, background work) produce output *eventually*, so "done" must be detected, not assumed. Snapshotting before the trace is complete yields a false golden — the worst failure mode.

Completion is decided by three signals, in order of authority:

1. **Expected-exit markers (primary).** The flow declares its expected exits — its I/O contract: e.g., "a `PUBLISH loan.approved` span," "a DB write to `ledger`," "an HTTP 200 return." Wait until all declared markers are observed. This ties completion to the outputs you're verifying anyway, which is exactly the I/O framing — and it's far more precise than guessing from silence.
2. **Quiet-period backstop.** After markers are seen, require no new spans for a quiet interval, to catch trailing/unexpected spans.
3. **Hard timeout → fail loudly.** If markers aren't all seen by the deadline, the test **fails** (it does not snapshot a partial trace), with a minimum-span-count sanity check as a second backstop.

```
await(scope, expectedExits, quiet, timeout):
    deadline = now + timeout
    loop:
        spans = currentSpans(scope)
        if rootEnded(spans) and allSeen(spans, expectedExits) and idleFor(spans, quiet):
            return spans, complete=true
        if now > deadline:
            return spans, complete=false   // FAIL — truncated, do not snapshot
        sleep(poll)
```

**In-process simplification:** for a synchronous in-process flow, completion ≈ "the handler/consumer call returned." The one trap: a flow that spawns a **fire-and-forget goroutine** (e.g., publishing after the response) may not have that span recorded when the handler returns — so even in-process needs a short quiet-period drain when background work exists.

---

## 5. Determinism & fidelity preconditions

These make the captured trace both reproducible (so canonicalization yields a stable golden) and faithful to what the service really does:

- **`AlwaysSample`** on the test pipeline — probabilistic sampling would drop spans nondeterministically.
- **Fixed test data** — seeds and fixtures pinned, so the path and cardinality don't vary run to run.
- **Stable test environment** — so *environmental* retries (flaky network/contention) don't appear; *intended* retries (a resilient flow) are legitimately part of the trace and stay.
- **Drive through the instrumented stack.** The trace is production-faithful only if the flow exercises the service's real entry points — the actual router with its middleware, the real consumer deserialization path — not handler functions called directly, which skip the server span and middleware spans and diverge from production.
- **A real datastore is recommended, not required.** With a fake DB (sqlmock, or SQLite standing in for Postgres) the captured DB operations are tautological or dialect-wrong, so that portion of the snapshot is thin or fictional. Because the artifact reflects *tested behavior* (§6), that thinness is itself feedback rather than a blocker — but a real Postgres (testcontainers) is what makes the DB portion trustworthy.
- **A controlled clock** wherever business logic is time-dependent ("after 5pm, route differently"), so the path doesn't vary by wall-clock. In-process is otherwise *more* deterministic than out-of-process — one clock domain, no network jitter.

---

## 6. What the captured trace reflects (and its limits)

- **The contract: it is a faithful function of the *test*, not a proxy for production truth** — "this is what your tests exercise." A thin or unrealistic test yields a thin or unrealistic artifact, and the gap between that and what you expect is the signal to write a better test. The snapshot doubles as behavioral-shape feedback on test quality (see the scope & guarantees doc).
- **Per-flow and sampled.** The snapshot captures only the branches the test's doubles drive; an event published only on an error branch is absent from a happy-path golden. So this artifact answers "what *did* happen on this flow," while "what *can* happen across all paths" is the static boundary's job — and the gap between them is the coverage signal.
- **Error paths are separate flows.** Each failure mode is its own flow and golden, which needs a way to deterministically drive the failure — a test seam, a boundary mock returning the error, or a fault-injection proxy. (The injection mechanism is itself an open choice; happy-path ships first.)
- **Distinct from out-of-process E2E capture.** The same logical flow captured in-process (mocked deps) and out-of-process (real deps, real downstream spans, real latency) produces *different* snapshots — the post-hoc path even gates a different artifact (the boundary-effect set, not the ordered tree; see `design/post-hoc-behavioral-ingestion.md`). They are distinct artifacts with distinct scopes — don't make one golden serve both, or you recreate the identity drift the consistency pass cleaned up.

---

## 7. Handoff to canonicalization

```go
type TriggerKind string // "http" | "event"
type CaptureMode string  // "in-process" | "post-hoc"

// CapturedFlow is the harness's output and canonicalization's input.
type CapturedFlow struct {
    Flow     string      // stable id (the test name)
    Trigger  TriggerKind
    Mode     CaptureMode
    Spans    []Span      // scoped to exactly this flow
    Root     *Span       // reconstructed entry (server or consumer span)
    Complete bool        // false => truncated/timed out; canonicalization must refuse
}
```

`Complete=false` is a hard stop — canonicalization's §3.1 completeness check is the last line of defense; this flag is the first. The harness reconstructs `Root` and the tree from `parent_span_id` and hands the whole thing over; canonicalization does the rest.

---

## 8. Build vs. buy

The post-hoc half — collector pipeline, trace-by-ID fetch, span selection/assertion — is largely what **Tracetest** already does (trigger a flow, pull the trace from your backend, assert on spans). It's a reasonable buy for the *capture + correlation* layer if you'd rather not build it; you'd still own canonicalization, the golden lifecycle, and rendering. The in-process half is small enough to own outright (in-memory exporter + the quiescence loop above).

---

## 9. Open decisions

- **Expected-exit declaration** → *resolved* by the config spec: the flow's exit markers are declared **with the test** (co-located, drift-proof), not derived from a first golden.
- **Real-bus vs. direct-injection** for consumer-rooted flows — fidelity vs. speed; per the instrumented-entry-point principle (§5), prefer the real consume path, with raw handler invocation as the explicitly-lower-fidelity shortcut.
- **Fault-injection mechanism** for error-path flows (§6) — test seam vs. boundary mock vs. proxy; deferred with the error paths themselves to v1.1.
- **Post-hoc store adapter** — which backend (Jaeger/Tempo/OTLP) the fetch targets; pluggable, but the first adapter is a concrete choice tied to your infra.
