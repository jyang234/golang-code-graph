# Post-hoc e2e reference (known-good end to end)

A runnable, verified reference for the out-of-process path: a real OTel Collector
+ an OTLP-exporting service → `flowmap behavior ingest`. Use it to **diff your own
setup against a working one** — it de-risks the two steps adoption usually
stalls on: the collector config and the `baggagecopy` wiring.

```
loansvc-otlp (:8080)                      otel-collector
  entry span + child spans      OTLP        receive
  AlwaysSample              ───────────▶   tail_sampling{flowmap.flow present}
  baggagecopy: flowmap.flow                file exporter (OTLP/JSON) → ./traces
  → onto every span                                 │
        ▲ baggage: flowmap.flow=…                    ▼
   curl / withFlow                      flowmap behavior ingest → boundary effects
```

## Run it

**No Docker** (needs `otelcol-contrib` on PATH + Go):

```sh
./run-local.sh
```

**Docker:**

```sh
docker compose up -d --build
curl -X POST -H 'baggage: flowmap.flow=loan-application' localhost:8080/loan-application
sleep 3                                   # batch export + tail_sampling decision_wait
flowmap behavior ingest examples/posthoc-e2e/traces/e2e.otlp.json
docker compose down
```

Either way you should see:

```
ingest: 1 flow fragment(s) from 6 span(s):
  - loan-application         [loansvc   ] 4 boundary effect(s)

boundary effects exercised (4):
  HTTP GET credit-bureau /score/{id}
  HTTP POST /loan-application
  HTTP POST payment-gw /charge/{id}
  PUBLISH loan.approved
```

## The three things to copy into your services

Everything that matters is in `service/main.go`, marked (1)–(3):

1. **OTLP exporter + AlwaysSample.** flowmap-tagged flows must be 100% sampled;
   probabilistic sampling silently truncates the capture.
2. **Trace-context + baggage propagation.** The composite propagator
   (`TraceContext{}, Baggage{}`) on the inbound seam, so the `flowmap.flow` tag
   the caller sets survives the hop. In a real service this is your `otelhttp`
   server handler.
3. **`baggagecopy` span processor — the load-bearing step.** Baggage is *not* in
   exported spans; the collector's `tail_sampling` and flowmap's grouping both
   key on span *attributes*. `baggagecopy.NewSpanProcessor` promotes
   `flowmap.flow` (and `Correlation-Id`) onto every span at start. **Omit it and
   nothing is selectable** — the single most common adoption failure.

The collector side is `otel-collector.yaml` (the self-contained form of the
additive blocks in `../../docs/integration/otel-collector.flowmap.yaml`):
`tail_sampling` keyed on the `flowmap.flow` attribute → `file` exporter as
OTLP/JSON.

## Graduate to the gate

```sh
# snapshot the effect set + rendered view, commit the flows you want gated:
flowmap behavior ingest --flows-dir flows/ --update <traces>
# CI enforces it (fails only on a NEW boundary effect):
flowmap behavior ingest --flows-dir flows/ <traces>
```

## Notes

- `service/` is its own Go module, so its OTel SDK / contrib deps stay out of
  flowmap's engine graph — the same isolation an adopting repo gets.
- The captured trace file under `./traces` is git-ignored: trace ids and
  timestamps are per-run. The committed deterministic sample for flowmap's own
  tests lives at `../../testdata/otlp/loansvc.collector.otlp.json`.
