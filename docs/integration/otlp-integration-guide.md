# Integration guide: flowmap on an existing OTLP e2e suite

This is the step-by-step for adding flowmap's post-hoc behavioral mapping to an
e2e suite you **already run** — Dockerized services emitting OTLP through a
collector, driven by an existing test runner (Playwright, k6, Go, whatever). You
do not rewrite tests or change how services emit; you tap the collector and read
the traces after the run.

- Conventions & copy-ready snippets: `README.md` (this directory).
- A runnable, verified end to end to diff against: `../../examples/posthoc-e2e/`.
- Why it works the way it does: `../design/post-hoc-behavioral-ingestion.md`.

> **Mental model.** flowmap does not need your e2e run to be reproducible. It
> reads the captured trace **file** — a fixed input — and canonicalizes it
> deterministically. So adding this is additive and cannot flake your existing
> suite: stage 1 is read-only reporting (always exits 0), and gating is opt-in,
> per flow.

---

## Before you start: map your setup

| flowmap needs | you probably already have | gap to close |
|---|---|---|
| services emit OTLP | OTLP → collector → Jaeger | none |
| a collector you can add an exporter to | the collector | one additive exporter (step 1) |
| each flow's spans tagged + selectable | trace context propagation | `flowmap.flow` tag + **baggagecopy** (steps 2–3) |
| 100% sampling for tagged flows | head/tail sampling | a tail policy or AlwaysSample (step 4) |
| a post-run step | your CI e2e job | one `flowmap behavior ingest` call (step 5) |

If your services are instrumented for only *some* boundaries (e.g. messaging via
`otelaws` but not HTTP/DB), flowmap will map exactly what's instrumented — bus
events now, HTTP entrypoints and DB once you add `otelhttp`/`otelsql`. You can
start with what you have.

---

## Step 1 — tap the collector (additive)

Add a second traces pipeline that keeps only flowmap-tagged traces and writes
them as OTLP/JSON. Your existing `jaeger`/`debug` pipeline is untouched and runs
in parallel. Merge the marked blocks from `otel-collector.flowmap.yaml`, or copy
the self-contained `../../examples/posthoc-e2e/otel-collector.yaml`:

```yaml
processors:
  tail_sampling/flowmap:
    decision_wait: 10s        # ≥ your slowest flow
    policies:
      - name: keep-flowmap-tagged
        type: string_attribute
        string_attribute: { key: flowmap.flow, values: [".+"], enabled_regex_matching: true }
exporters:
  file/flowmap:
    path: /var/lib/flowmap/traces/e2e.otlp.json
    format: json
service:
  pipelines:
    traces/flowmap:                       # NEW — your existing `traces` pipeline stays as-is
      receivers: [otlp]
      processors: [tail_sampling/flowmap]
      exporters: [file/flowmap]
```

Mount the output path to somewhere your CI step can read after the run.

## Step 2 — promote `flowmap.flow` onto spans (the load-bearing step)

**Baggage is not in exported spans.** It rides as a propagation header; the
collector's `tail_sampling` and flowmap's grouping both key on span
**attributes**. So each service must copy `flowmap.flow` (and your
`Correlation-Id` if you propagate one) from baggage onto every span at start,
with the OTel `baggagecopy` span processor:

```go
import "go.opentelemetry.io/contrib/processors/baggagecopy"

tp := sdktrace.NewTracerProvider(
    sdktrace.WithSpanProcessor(baggagecopy.NewSpanProcessor(func(m baggage.Member) bool {
        return m.Key() == "flowmap.flow" || m.Key() == "Correlation-Id"
    })),
    // … your existing batch/exporter processors …
)
```

Also ensure the propagator carries baggage end to end (it often already does):

```go
otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
    propagation.TraceContext{}, propagation.Baggage{}))
```

**Without this step, `flowmap.flow` never reaches the collector and nothing is
selectable** — the single most common adoption failure. See it wired in
`../../examples/posthoc-e2e/service/main.go`, points (2)–(3).

## Step 3 — tag flows in your existing tests (one line each)

Set the `flowmap.flow=<slug>` baggage member for the duration of one flow.
`<slug>` becomes the golden's name. In TypeScript/Playwright use `withFlow.ts`:

```ts
await withFlow("publish-fanout", async () => {
  await request.post("/loan-application", { data: { amount: 5000 } });
  // … your existing e2e assertions, unchanged …
});
```

In any client, this is just the W3C `baggage` header on the requests:
`baggage: flowmap.flow=publish-fanout`.

## Step 4 — sample tagged flows at 100%

Probabilistic sampling silently truncates a capture. Either head-sample tagged
requests at `AlwaysSample`, or rely on the `tail_sampling/flowmap` policy from
step 1 (it keeps every trace carrying the attribute). Don't leave tagged flows
subject to a 1% head sampler.

## Step 5 — ingest after the e2e run (stage 1, non-gated)

Add one step to your CI e2e job, after the run drains:

```sh
make -C infra test/e2e/<svc>                 # your existing e2e run
# drain the collector so the file is complete: SIGTERM-to-flush, or wait
# ≥ decision_wait + batch interval before reading.
flowmap behavior ingest --service-dir services/<svc> \
    /var/lib/flowmap/traces/e2e.otlp.json
```

This prints the boundary effects your e2e actually exercised and the coverage
delta against the service's static boundary contract. **It always exits 0** —
pure feedback, zero flakiness risk. Ship this first; it harvests your existing
e2e investment immediately and gives teams a reason to add the tag.

To **see the cross-service choreography** at this stage — before committing any
golden — add `--render-dir`; it writes a `<slug>.system.flow.md` per flow (the
whole-flow diagram across every service the flow touched), in any mode:

```sh
flowmap behavior ingest --render-dir e2e-diagrams/ /var/lib/flowmap/traces/e2e.otlp.json
# center the diagram on one service's subtree (what it touches downstream):
flowmap behavior ingest --render-dir e2e-diagrams/ --root payments-svc /var/lib/flowmap/traces/e2e.otlp.json
```

> **Flags precede the trace path** (`flowmap behavior ingest [flags] <path>`).
> `<path>` may be a file or a directory of rotated `*.json`.

## Step 6 — graduate a proven flow to the gate (opt-in)

Once a flow reliably exercises a stable set of boundary effects:

```sh
# snapshot its effect set + rendered views; commit the ones you want gated.
flowmap behavior ingest --flows-dir flows/ --update <traces>
git add flows/<slug>.<svc>.effects.json flows/<slug>.<svc>.flow.md

# CI enforces every committed golden (no --update):
flowmap behavior ingest --flows-dir flows/ <traces>
```

`--update` also writes a **cross-service** `flows/<slug>.system.flow.md`: the
whole-flow choreography across *every* service the flow touched (not split per
service), so you can see everything the flow interacts with end to end. Only the
per-service `*.effects.json` gates; the `.flow.md` views (per-service and system)
are committed for human review.

The gate fails **only on a new boundary effect** (`[CONTRACT] ADDED …`) — a new
published event, dependency, or entrypoint. A missing/under-exercised effect is
reported, never failed (so a run that exercises less doesn't flake). A golden
with no capture this run is **skipped, not silently passed**. Route
`flows/*.effects.json` through CODEOWNERS so a contract change reaches a human.

---

## Verify it works

After step 5, you should see (shapes, not exact values):

```
ingest: N flow fragment(s) from M span(s):
  - publish-fanout          [your-svc ] K boundary effect(s)

boundary effects exercised (K):
  PUBLISH …
  HTTP …
```

If you see `0 span(s)` or `none tagged flowmap.flow`, work the table below.

## Troubleshooting (the failure modes we hit building this)

| Symptom | Cause | Fix |
|---|---|---|
| `none tagged flowmap.flow` (spans present) | baggagecopy missing, or baggage not propagated | add the `baggagecopy` processor (step 2); ensure `Baggage{}` is in the propagator and the test sets the baggage header |
| `0 span(s)` / empty file | service didn't export, or collector didn't flush | service export `404`? use `OTEL_EXPORTER_OTLP_ENDPOINT` (base URL — the SDK appends `/v1/traces`), not the `_TRACES_ENDPOINT` form; drain the collector before reading |
| effect like `HTTP POST POST /x` (doubled) | `http.route` includes the method | set `http.route` to the path template only (`/x`); the method is a separate attribute |
| gate says `no capture this run — skipped` | that flow's trace wasn't captured (under-sampled, untagged, or truncated) | this is **not** a pass — fix sampling/tagging so the flow is captured, or the drain timing |
| fewer effects than expected | instrumentation breadth | add `otelhttp`/`otelsql`; `otelaws`-only yields bus events only |
| file has partial traces | read before the collector flushed | wait ≥ `decision_wait` + batch interval, or SIGTERM the collector to flush, before `ingest` |

## Reference

A complete, **verified** version of all of the above — real collector, real
`baggagecopy`, real file export → `ingest` — runs with one command:

```sh
examples/posthoc-e2e/run-local.sh      # or: docker compose up in that directory
```

Diff your collector config and service wiring against
`examples/posthoc-e2e/` whenever something doesn't match.
