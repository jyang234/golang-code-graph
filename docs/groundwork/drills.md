# Triage effectiveness drills — the record

The incident-triage plan (§7, E1–E4) demands that triage's value be *measured*
against named keep/kill thresholds, not asserted. E1–E3 are simulated against
the loansvc dogfood fixture and run as **committed tests**
(`internal/groundwork/impact/drill_test.go`), so the thresholds are
assertions: triage quality is a ratchet, and a change that erodes recall or
scoping power fails the suite. Re-run and reprint the numbers any time with:

```console
$ go test ./internal/groundwork/impact/ -run TestDrill -v
```

## E1 — staged incidents: recall and scoping power

Ten realistic alert symptoms, each with a hand-labeled true culprit (chosen
by reading the fixture, never the tool's output), spanning every symptom kind:
two failing peers, two corrupted tables, a missing published event, a missing
*dynamically-named* event (the flagged-possible path), a starved consumer, two
route alerts (one exact, one with a mount prefix and a concrete path id — the
forms a Dynatrace problem actually reports), and a panic frame pasted in
Dynatrace's runtime form.

| Measure | Result | Threshold |
|---|---|---|
| Recall (culprit ∈ suspect set) | **10/10 (100%)** | keep: 100% — a miss is a resolver defect |
| Median hunt space (suspects ∪ upstream callers) | **8% of the graph** (3 of 39 nodes) | kill: ≥50% ("the card narrows nothing") |
| Sharpest scenarios | the route alerts, at 3% (a route maps straight to its handler) | — |
| Worst-case hunt space | 15% (the starved consumer — its suspect set now includes the actual handler via the entrypoints join, not just the registration site) | — |

Reading: on this fixture, a responder (or agent) starts an incident with the
hunt already narrowed from 39 functions to a median of three, and the true
culprit has never been outside the set. The honest caveat: loansvc is small
and well-factored; the fractions will grow on monolithic graphs — which is
exactly why the threshold is committed as an assertion rather than a one-time
claim.

## E2 — the graph-to-trace handoff

"Graph to narrow, telemetry to locate," verified end-to-end: the committed
collector capture (`testdata/otlp/loansvc.collector.otlp.json`) is staged
into an incident trace by dropping the payment-gateway charge spans — the
request "failed at the charge" — and run through the real post-hoc pipeline
(`otlpjson` → `ingest` → `canon`).

- The trace analysis **located the divergence**: `HTTP POST payment-gw
  /charge/{id}` missing from the observed effect set.
- The divergence's producing function, `(*client.Gateway).Charge`, is
  **inside the one-function suspect set** the graph card bounded for the
  matching symptom (`--peer payment-gw`).

The two lenses compose: the card scopes the hunt, the incident's own trace
pinpoints the divergence inside it, and no test case was authored.

## E3 — the staleness demonstration

Triage with a graph one commit behind a routing change mis-scopes,
deterministically: a deployed commit adds a `Refund` route that charges the
gateway; the stale card names **1** degraded entrypoint where the deployed
card names **2** — the new route is invisible to the stale map. This converts
the graph-per-deploy prerequisite from an assertion into evidence, and is the
mis-scope `flowmap graph --stamp` / `groundwork triage --expect` exists to
catch loudly.

## E4 — the agent comparison (not simulated; by design)

E4 asks whether an agent with the MCP tools identifies and fixes a staged
incident better than one with only the raw repo. That requires live agent
sessions and qualitative judgment — simulating it inside the test suite would
put an AI in the measurement loop of a framework whose premise is that no AI
sits in any verdict. The design stands as specified in the triage plan:
~10 staged incidents, with/without the `groundwork mcp` server, measuring
whether conclusions cite card facts, exploratory query counts, and
first-attempt `verify` block rates on the fix. Run it as a human-judged
exercise when adopting; record results here.

The quantitative half is instrumented: run the server with `--log
calls.jsonl` and read it with `groundwork transcript calls.jsonl [--json]` —
sessions and per-session query counts, the tool and service mix,
cross-service hops (do agents actually walk publisher→consumer across
services?), and error/correction rates. The transcript is deterministic
(no timestamps), so a replayed drill produces identical bytes and the
summary is diffable evidence. The card states its own limit: counts measure
usage, not value — whether conclusions cite card facts stays human-judged.

## Standing limitations these drills do NOT cover

Causes outside the code entirely (the fault card's scope statement exists for
this), cross-function effect orderings (disclosed on every fault card), and
routes registered through routers outside root discovery's coverage (gin's
variadic handlers, gorilla's .Methods() chains, gRPC) — those routes are
absent from the entrypoints section, so a route query against them is a loud
no-match, never a guess. The drills measure that triage does its job well —
not that its job covers everything.
