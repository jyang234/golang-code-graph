# Post-hoc behavioral ingestion (`ModePostHoc`) — design brief

**Status:** stage 1 landed (the non-gated coverage view); stage 2 (set-based
gating) proposed. Implements the deferred Phase 10 (`implementation_plan.md`) and
extends the determinism contract (`trace-canonicalization-spec.md §1`) to
out-of-process capture.

**Built so far** (`flowmap behavior ingest <traces> [--service-dir D]`):
`internal/otlpjson` (OTLP/JSON → `capture.Span`, no gRPC/pdata dep),
`internal/ingest` (slug × service grouping + per-service root assembly, design
D-PH1/D-PH4), and the `behavior ingest` CLI verb that canonicalizes each
fragment through the **unchanged** `canon` and prints the exercised
boundary-effect set + the `coverage.Delta` against a service's boundary
contract. Always exits 0. Deferred: the post-hoc canon profile knobs ([P10.3]
redaction/op-key ordering), and stage-2 `--update`/golden set-comparison.

**Audience:** the flowmap team (the build asks below) and adopting service teams
(the conventions in §4 and `docs/integration/`).

---

## 0. The principle: observe, don't gate (at first)

Today flowmap captures **in-process**: the harness re-drives a flow through the
real router under an in-memory OTel pipeline and the snapshot-assertion gate
re-runs it 3× and demands byte-identical canonical IR (`flow/flow.go:182`). That
self-test exists *only because the harness drives the flow*. Out-of-process there
is nothing to re-drive — the captured trace file is a **fixed, immutable input**,
and `canon` is a pure function of it. So the two notions of determinism separate
cleanly:

- **flowmap's processing is deterministic** — `canon(file)` is reproducible. ✅
  This is all the gate needs.
- **the e2e run is reproducible** — it is not, and need not be. The real
  distributed run has wall-clock skew, async export, scheduling, and
  data-dependent volume. None of it reaches the snapshot, because `canon`
  discards exactly those dimensions (`trace-canonicalization-spec.md §1`).

This is why the in-process gate model does **not** conflict with out-of-process
capture: the conflicting machinery (the 3-run self-test, byte-exact golden
compare, hard cardinality) is the *verdict* layer, not the *mapping* layer. We
ship the mapping layer first, **un-gated**, and graduate individual proven-stable
flows to a set-based gate later (§6).

---

## 1. The integration shape

Tap the collector you already run; process post-hoc off a file. Services keep
emitting exactly as they do; one additive collector exporter; one-line per-test
authoring.

```
Playwright e2e test
  └─ withFlow("publish-fanout", …)        ← sets baggage flowmap.flow=<slug>
     └─ HTTP → Dockerized services        [otelaws/otelhttp/otelsql spans]
        │                                   ⚠ each service runs a baggagecopy
        │                                     span processor (see §4) so
        │                                     flowmap.flow lands on the SPANS
        └─ OTLP → otel-collector
           ├─ otlp/jaeger, debug          (unchanged)
           └─ NEW: tail_sampling{flowmap.flow present} → file exporter (OTLP-JSON)
                 → e2e/<svc>/.flowmap/traces/*.json

── after the e2e run (a CI step) ──
flowmap behavior ingest <traces> --flows-dir flows/ [--update]
   ├─ group spans by trace-id; keep flowmap.flow-tagged; golden named by slug
   ├─ OTLP-JSON → capture model → canon(post-hoc profile) → ir.CanonicalTrace
   ├─ (stage 1) emit coverage view: boundary effects the e2e actually exercised
   └─ (stage 2, opt-in) compare vs committed *.golden.json (set semantics);
        CODEOWNERS routes golden changes to a human
```

Zero change to how services emit. The Jaeger/debug pipelines are untouched. The
only per-test authoring is the one-line tag.

---

## 2. Build asks for flowmap

### [P10.1] Post-hoc OTLP ingestion — implement `ModePostHoc`
Read OTLP/JSON (the collector file exporter's output) and adapt it into the
**existing** `capture.CapturedFlow` model — the same seam the in-process harness
feeds (`harness/harness.go:325 fromOTel`). Everything downstream (`canon`, `ir`,
`diff`, `render`, `coverage`) is then reused unchanged (decision D8). New CLI:

```
flowmap behavior ingest <file|dir> [--flows-dir D] [--update]
```

Goldens keyed by flow slug; `flowmap diff` / `coverage` / `*.flow.md` work as-is.

> **Keep the public module lean.** `harness`/`flow`/`ir` are semver-stable and
> imported by every adopting service ([C1]); they must **not** grow a gRPC /
> `pdata` / collector dependency. Read OTLP/JSON with a small **internal**
> decoder (`internal/otlpjson`) that handles the two awkward parts — base64/hex
> trace & span ids, and the `AnyValue` attribute union — and emits
> `[]capture.Span`. Put the CLI verb in `cmd/flowmap`; keep the receiver out of
> the public surface entirely (file-based, no network server).

### [P10.2] Flow scoping over a trace file
The out-of-process analog of `capture.Scope` (`capture/capture.go:107`). A
`withFlow(slug)` block can issue several top-level requests, each rooting its own
trace, so **one slug spans multiple traces**. Two reductions of the same spans,
for two consumers (resolved decision D-PH1):

- **Assertion / coverage unit = the slug.** Union the boundary effects across all
  traces carrying `flowmap.flow=<slug>`; that union is the gated set.
- **Diagram unit = one representative trace.** Pick one trace (the largest) per
  slug for the `*.flow.md`, noted as illustrative — the diagram shows a single
  execution, the assertion covers the flow class.

Select only spans carrying a `flowmap.flow` attribute; support `Correlation-Id`
as an alternate key (already propagated today). A trace with no `flowmap.flow`
tag is ignored, not an error — the file may carry unrelated traffic.

### [P10.3] Post-hoc canonicalization profile — *the hard, essential part*
Out-of-process traces carry nondeterminism the in-process path never sees. A
`canon` profile (a config flag, not a fork) handles it:

- **Redaction, extended.** Strip/normalize `host.*`, `*.pod.*`, `*.port`,
  `service.instance.id`, `process.*`, k8s/instance attributes, on top of the
  existing UUID/timestamp/numeric-id redaction (`canon §3.2`).
- **Ordering — keep what survives, drop only the in-process signals.**
  `parent_span_id` and span links **survive in OTLP**, so causal (parent→child)
  happens-before is still free; keep it. What does *not* survive is the in-process
  sibling signal — `flowmap.goid` and same-process caller-clock intervals
  (`capture.Concurrent`, `capture.go:164`). So in this profile, **ambiguous
  siblings order by canonical op-key**, and `canon §3.3`'s "never compare
  cross-service timestamps" / "default to concurrent on ambiguity" rules carry
  the rest. Net: the readable sequence (charge→publishes→ledger) is preserved
  where it is causal; only genuinely-concurrent peers fall back to op-key order.
- **Gate on the boundary-effect *set*, not cardinality/ordering/timing.** The
  post-hoc assertion is the set of `boundaryKey`s the flow exercised — events
  published/consumed, deps called, entrypoints — **the same key space the
  coverage join already defines** ([H2], `implementation_plan.md:186`). The full
  ordered tree remains the human view (`*.flow.md`), but the *assertion* is
  set-based, which is what keeps the gate from flaking. `ExpectExactlyOnce`-style
  cardinality is **opt-in per flow** (§5), for flows that are genuinely
  deterministic.
- **Comparison = no-new-effects, not set equality** (resolved decision D-PH3).
  The gate fails only on a **new** boundary effect in the observed set vs. the
  golden (a new published event / dep / entrypoint — the contract change worth
  catching). A **missing / under-exercised** effect does **not** fail the gate —
  a distributed run legitimately under-exercises a flow — it surfaces in the
  coverage view instead. This tolerates run-to-run flakiness in one direction
  (volume) while still catching the additions that are real contract drift.

> **Free consequence.** Because the post-hoc artifact lives in the coverage key
> space, ingestion feeds `coverage.Delta` directly. The stage-1 non-gated view
> ("boundary effects your e2e actually exercised") is therefore *nearly free*
> once [P10.1]/[P10.2] land — it is the union of ingested boundary keys minus the
> static boundary, rendered as coverage, exit 0.

### [P10.4] Tagging + collector conventions (a short spec, not code we import)
The `flowmap.flow` key, the recommended collector `tail_sampling`+file-export
config, the **"flowmap-tagged flows are 100% sampled"** rule, and the
**baggage→span-attribute** requirement (§4). Shipped as `docs/integration/`.

---

## 3. Determinism: what is and isn't guaranteed post-hoc

| Dimension | In-process | Post-hoc |
|---|---|---|
| canon over a fixed file | deterministic | **deterministic** (same guarantee) |
| sibling order signal | goroutine + caller-clock | op-key only (causal order kept) |
| capture completeness | quiescence + re-drive | root+markers present ⇒ gate; else skip-with-warning |
| gate contract | full ordered tree, byte-exact | boundary-effect set, no-new-effects (cardinality opt-in) |
| self-test (3× re-drive) | required | **N/A** — file is the fixed input |

The post-hoc golden is therefore a **different artifact** from an in-process
golden of the same flow (set vs. ordered tree, more concurrent groupings). They
are not interchangeable and must not be diffed against each other — name them in
distinct directories.

---

## 4. Conventions (the §[P10.4] spec, summarized)

1. **`flowmap.flow=<slug>`** — a baggage member set by the test (`withFlow`), and
   the golden's name. Its presence on a trace is also the tail-sampling selector.
2. **⚠ Promote baggage onto spans.** Baggage is a propagation header; it is **not
   in the exported OTLP spans**. The collector's `tail_sampling` and flowmap's
   grouping both key on **span attributes**, so each service must run a
   **`baggagecopy`** span processor
   (`go.opentelemetry.io/contrib/processors/baggagecopy`) that copies
   `flowmap.flow` (and `Correlation-Id`) onto every span at start. This is the
   out-of-process analog of the in-process `startProcessor`
   (`harness/harness.go:128`). **Without it the tag never reaches the collector
   and nothing is selectable** — the most common adoption failure.
3. **100% sampling for tagged flows.** Head-sample tagged requests at
   `AlwaysSample`, or use a `tail_sampling` policy keyed on the `flowmap.flow`
   attribute. Probabilistic sampling drops spans non-deterministically and
   silently truncates the capture.
4. **File export is additive.** A second exporter on the traces pipeline; the
   `otlp/jaeger` and `debug` exporters are untouched.

See `docs/integration/` for a ready-to-copy collector config and `withFlow`
helper.

---

## 5. Decisions

### Resolved
- **D-PH1 — grouping unit.** Slug for the assertion/coverage set; one
  representative trace for the diagram (§[P10.2]).
- **D-PH2 — completeness.** Stage 1 trusts the file but reports span/trace counts
  and "no root span" warnings, never fails. Stage 2 gates a flow only when its
  service-entry root + declared markers are present in the file (reuse `await`'s
  marker logic statically); otherwise skip-with-warning rather than gate on a
  possibly-truncated trace.
- **D-PH3 — comparison.** No-new-effects (§[P10.3]); missing effects go to
  coverage, not the gate.
- **D-PH4 — multi-service.** Per-service split: partition a cross-service trace
  by `service.name`, scope each service to its own spans, validate against that
  service's golden. Matches the boundary-contract model; each team owns its
  golden. Naming: `<slug>.<service>.golden.json`. (Whole-flow choreography is the
  separate Phase 13 cross-repo concern.)
- **D-PH5 — expectations location.** Snapshot/set-only to start — the golden is
  the assertion. A small optional per-flow `expectations.yaml` (opt-in
  cardinality) is added only once a flow has earned it.

### Still open before the build (mechanical / your-side)
- **OTLP-JSON format.** Capture one real collector `file`-exporter sample and pin
  the decoder against it **before** writing `internal/otlpjson` — OTel encodes
  trace/span IDs as **hex**, not the proto-JSON base64 default, and the
  `AnyValue` attribute union needs explicit handling. Decide hand-roll (lean) vs.
  `pdata`'s `ptrace.JSONUnmarshaler` (robust, heavier — isolate it off the public
  `harness`/`flow`/`ir` surface either way).
- **CODEOWNERS.** Route the new post-hoc golden paths
  (`**/.flowmap/**/*.golden.json` under the e2e dirs) before stage 2.
- **Instrumentation breadth (your side).** `otelaws`-only today ⇒ stage 1 shows
  published/consumed events (the bus fan-out) but not HTTP entrypoints / DB until
  `otelhttp`/`otelsql` are added. Confirmed worth shipping bus-only first.
- **Collector drain in CI (your side).** The file is complete only after
  `tail_sampling`'s `decision_wait` elapses **and** the exporter flushes. CI must
  drain the collector (SIGTERM-to-flush, or wait ≥ `decision_wait` + batch)
  before `ingest` reads, or it races the late-decided traces — the same
  truncation risk as D-PH2 from the producer end.

---

## 6. Staging — the DX-safe rollout

1. **Ship [P10.1]+[P10.2]+[P10.3] non-gated**, as the coverage view ("boundary
   effects the e2e actually exercised"). Harvests the existing e2e investment
   immediately, **zero flakiness risk**, and gives teams a concrete reason to add
   the `flowmap.flow` tag.
2. **Graduate individual proven-deterministic flows to set-based gating**, routed
   through CODEOWNERS. The sequencing is itself the adoption on-ramp.

This ordering keeps the gate question (§0) entirely inside stage 2, where it
belongs, and never blocks a build on day one.
