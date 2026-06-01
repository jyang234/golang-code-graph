# Adopting flowmap in a service

flowmap verifies what your service does to the world — the events it
publishes/consumes and the services it calls — and gates unintended changes to a
human reviewer. This is the end-to-end recipe for wiring it into a Go service.

The fixture under `testdata/fixtures/loansvc` is a complete, working example of
everything below.

---

## 1. Instrument the service with OpenTelemetry

flowmap reads OTel spans. Instrument your real router, DB layer, outbound
clients, and bus the way you already would for tracing — obtain the tracer with
`otel.Tracer(...)` (do **not** cache it at package init; fetch it per span so it
binds to whatever provider is installed). Set the standard semantic-convention
attributes:

- HTTP server/client: `http.request.method`, `http.route`, `peer.service`
- DB: `db.system`, `db.statement`
- messaging: span kind producer/consumer + `messaging.destination.name`

That is the only production-code change. flowmap's analysis and harness are
test-time.

## 2. Commit the gated boundary contract

```sh
flowmap boundary .          # regenerate .flowmap/boundary-contract.json
flowmap boundary --check .  # CI runs this: fails if the committed copy is stale
```

Commit `.flowmap/boundary-contract.json` alongside the code. A new
published/consumed event or external dependency changes it and routes to review.
The non-gated full call graph is available via `flowmap graph .`.

## 3. Write a flow test

Flow tests live in a `flows/` package and use only the public `harness` and
`flow` packages. The committed goldens land in `flows/testdata/flows/`.

```go
package flows_test

import (
	"testing"
	"time"

	"github.com/jyang234/golang-code-graph/flow"
	"github.com/jyang234/golang-code-graph/harness"
)

func TestLoanApplication(t *testing.T) {
	app := harness.NewInProcess(t, newRouter(), harness.WithService("loansvc"))
	flow.New("POST /loan-application").
		TriggerBody("POST", "/loan-application", body).
		ExpectExactlyOnce("PUBLISH loan.approved").
		Expect("DB postgres INSERT ledger").
		Run(t, app)
}
```

- `NewInProcess` wires the real OTel pipeline; pass your real `http.Handler`.
- `Expect(op)` / `ExpectExactlyOnce(op)` declare the flow's expected exits using
  canonical op keys (`PUBLISH loan.approved`, `DB postgres INSERT ledger`,
  `HTTP GET credit-bureau /score/{id}`). They drive completion *and* the
  cardinality check.
- `Run` captures, canonicalizes, runs the determinism self-test, compares to the
  golden, and enforces cardinality.

Generate the goldens the first time (and after an intended behavior change):

```sh
go test ./flows/ -update    # writes *.golden.json + *.flow.md, then commit them
```

**Flows must be idempotent.** `Run` re-drives the flow 3× by default for the
determinism self-test (this is what varies goroutine scheduling), so its side
effects (DB writes, publishes) happen 3×. Use a fresh fixture/transaction per run,
or `flow.New(...).SelfTest(1)` to opt down to a single execution (trading
scheduling-variation coverage). Flow tests are parallel-safe — the harness
installs one process-wide OTel pipeline and isolates each flow by a unique
`test.run.id`, so `t.Parallel()` is fine.

A real datastore (testcontainers Postgres) makes the DB portion trustworthy; a
SQLite or fake-driver stand-in is fine for fast, hermetic runs — the snapshot is
a faithful function of the *test*, so a thin double yields a thin (but honest)
snapshot.

## 4. See what isn't covered

```sh
flowmap coverage .          # boundary effects no committed flow exercises
```

This is the emergent signal: "you publish `loan.declined` on a path no flow
drives." It is informational — a gap means *write a flow*, not *fail the build*.

## 5. Wire CI and CODEOWNERS

Two gate jobs (see `.github/workflows/gates.yml` for the working example):

- **currency-gate** — `flowmap boundary --check .` (the contract is a pure
  function of code).
- **snapshot-gate** — `go test ./...` (a stale golden fails the suite).

Route the gated artifacts and the per-flow tests to a human in `CODEOWNERS`:

```
**/.flowmap/boundary-contract.json   @your-team
**/.flowmap.yaml                     @your-team
**/testdata/flows/*.golden.json      @your-team
**/testdata/flows/*.flow.md          @your-team
**/flows/*_test.go                   @your-team
```

## 6. Configure classification (optional)

`.flowmap.yaml` names the libraries flowmap cannot infer — your bus client,
logger, DB layer, and outbound HTTP/RPC seam — plus any tier overrides. Standard
stdlib/OTel usage needs none of this; the common addition is naming your internal
bus client. See `testdata/fixtures/loansvc/.flowmap.yaml`.

Interface-dense services can raise the over-approximation flag threshold under
`static:`:

```yaml
static:
  highFanOutThreshold: 20   # default 8; flags dynamic-dispatch sites with more callees
```

---

## Scope (v1)

v1 gates **in-process, single-service** flows per MR — fast, deterministic, one
clock domain. Out-of-process and inter-service E2E (multiple services, clock
skew) are v2. The boundary contract is still exhaustive over statically-reachable
paths; what a flow *doesn't* exercise is exactly what `flowmap coverage` reports.
