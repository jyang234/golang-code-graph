# Integrating flowmap with an out-of-process e2e suite

Reference material for tapping an existing OTel collector and feeding captured
traces to `flowmap behavior ingest` (the post-hoc path — see
`docs/design/post-hoc-behavioral-ingestion.md`). Nothing here is imported by
flowmap; it is the **convention spec** plus copy-ready scaffolding for a service
team.

> **Integrating an existing e2e suite? Start with the step-by-step:**
> [`otlp-integration-guide.md`](./otlp-integration-guide.md). This README is the
> conventions + snippets it draws on.

These files are additive: your services emit exactly as they do today, and your
Jaeger/debug pipelines are untouched.

> **Want a working end to end first?** `examples/posthoc-e2e/` is a runnable,
> verified reference — a real OTel Collector + an OTLP-exporting service →
> `flowmap behavior ingest` — to diff your setup against. Start there if the
> collector/baggagecopy wiring is new to you.

---

## The three conventions

1. **Tag the flow.** The test sets a baggage member `flowmap.flow=<slug>` for the
   duration of one flow. `<slug>` is the golden's name. Use `withFlow.ts`.

2. **Promote baggage onto spans — the load-bearing step.** Baggage is a
   propagation header; it does **not** appear in exported OTLP spans. The
   collector's sampling and flowmap's grouping both key on **span attributes**,
   so every service must copy `flowmap.flow` (and your `Correlation-Id`) from
   baggage onto each span. Run the OTel **`baggagecopy`** span processor:

   ```go
   import "go.opentelemetry.io/contrib/processors/baggagecopy"

   tp := sdktrace.NewTracerProvider(
       sdktrace.WithSpanProcessor(
           baggagecopy.NewSpanProcessor(baggagecopy.AllowAllMembers),
           // or restrict to the flowmap keys:
           // baggagecopy.NewSpanProcessor(func(m baggage.Member) bool {
           //     return m.Key() == "flowmap.flow" || m.Key() == "Correlation-Id"
           // }),
       ),
       // … your existing exporter span processor …
   )
   ```

   Without this, `flowmap.flow` never reaches the collector and **nothing is
   selectable**. This is the most common adoption failure.

3. **100% sample tagged flows.** Either head-sample tagged requests at
   `AlwaysSample`, or rely on the collector `tail_sampling` policy below.
   Probabilistic sampling silently truncates the capture.

---

## Files here

- `otel-collector.flowmap.yaml` — the additive collector exporter + tail-sampling
  policy. Merge the marked blocks into your existing collector config.
- `withFlow.ts` — the one-line-per-test Playwright helper that sets the baggage.

---

## CI step (after the e2e run)

```sh
make -C infra test/e2e/<svc>          # your existing e2e run, writes traces/*.json

# stage 1 — non-gated, always exit 0: exercised effects (+ coverage delta).
flowmap behavior ingest --service-dir services/<svc> \
    src/test/e2e/<svc>/.flowmap/traces

# graduate a proven flow: snapshot its boundary-effect set + rendered view,
# then review and commit only the flows you intend to gate.
flowmap behavior ingest --flows-dir flows/ --update \
    src/test/e2e/<svc>/.flowmap/traces

# stage 2 — opt-in gate (CI): fails only on a NEW boundary effect in a
# committed flows/*.effects.json (no-new-effects). CODEOWNERS reviews the diff.
flowmap behavior ingest --flows-dir flows/ \
    src/test/e2e/<svc>/.flowmap/traces
```

Flags precede the trace path. `--service-dir` points at the service **source**
(flowmap generates its boundary contract for the coverage delta). A committed
`flows/<slug>.<svc>.effects.json` is what opts a flow into the gate; a golden
with no capture this run is skipped, never silently passed.
