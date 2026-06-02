# Integrating flowmap with an out-of-process e2e suite

Reference material for tapping an existing OTel collector and feeding captured
traces to `flowmap behavior ingest` (the post-hoc path — see
`docs/design/post-hoc-behavioral-ingestion.md`). Nothing here is imported by
flowmap; it is the **convention spec** plus copy-ready scaffolding for a service
team.

These files are additive: your services emit exactly as they do today, and your
Jaeger/debug pipelines are untouched.

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
flowmap behavior ingest src/test/e2e/<svc>/.flowmap/traces \
    --service-dir services/<svc>      # stage 1: exercised effects + coverage, exit 0
# stage 2 (opt-in, per proven flow — not yet built):
# flowmap behavior ingest … --update   # rebase goldens; CODEOWNERS reviews the diff
```

`--service-dir` points at the service **source** (so flowmap can generate its
boundary contract and show the coverage delta); omit it to just list the
exercised boundary effects. The ingest step never fails the build.
