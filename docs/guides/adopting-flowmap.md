# Adopting flowmap + groundwork in a service

> **`ACTIVE`** · adoption guide · _reviewed 2026-06-16_

This is the end-to-end recipe for wiring the toolchain into a Go service and into
your engineering lifecycle. The two tools split cleanly:

- **flowmap** *produces facts* — the static call graph with typed boundary
  effects, the gated boundary contract, path-obligation verdicts, and behavioral
  golden snapshots. It verifies what your service does to the world (events
  published/consumed, services called) and gates unintended changes to a human.
- **groundwork** *judges those facts* against a human-authored `policy.json` and
  puts them in front of every seat in the lifecycle — **design, build, review,
  triage** — without ever putting an AI in a verdict.

**Part 1** wires in flowmap (the producer). **Part 2** wires in groundwork (the
judge) and shows the lifecycle loop. The fixture under
`testdata/fixtures/loansvc`, plus the policy fixtures under `testdata/groundwork/`,
is a complete working example of everything below.

---

# Part 1 — flowmap: producing the facts

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
The non-gated full call graph is available via `flowmap graph .` (add
`--algo vta` to refine interface-dense dispatch; the default is `rta`). For a
human-readable view, `flowmap graph . --mermaid` renders it as a Mermaid
flowchart — typed boundary effects (DB / bus / external) as shaped leaf nodes,
and the blind spots and frontier markers as explicit terminal nodes so a
reviewer sees where the analysis stops. Scope it to one handler with
`--root "POST /loans"`, or color a base→branch delta with
`--diff base.graph.json`. A whole-service render is illegible, so above
`--max-nodes` (default 300) it emits an index of entry points to `--root` at
instead — `--root` is the reviewable unit. Like the JSON graph, the flowchart is
a view, never a gate.

**Mermaid compatibility.** The flowchart uses a deliberately conservative Mermaid
dialect — `flowchart`, `subgraph`, `classDef` + `:::` class assignment, the
`[(…)]` / `{{…}}` / `([…])` node shapes, `==>` / `-.->` edges, and `<br/>` in
labels — supported by mermaid ≥ 8.8 with HTML labels enabled (the default). That
covers GitHub's native rendering and GitLab ≥ 13.3; on older self-hosted GitLab,
upgrade mermaid or browse the `.md` as a build artifact. Data in labels is
HTML-escaped, both so it renders correctly and as a guard against markup
injection into a reviewer's rendered view. A test-suite "dialect floor" fails CI
if a less-portable construct is ever introduced.

**Output stability.** `--mermaid`, `--root`, `--diff`, `--show-plumbing`, and
`--max-nodes` are the committed CLI surface; treat the rendered Markdown as a
view (diff-reviewable, never gated), and pin a flowmap version in CI so the diff
sides share one renderer.

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

**HTTP routers.** Root discovery recognizes stdlib `ServeMux` and go-chi (so
oapi-codegen's chi *and* std-net/http servers work out of the box). For another
method-named router — echo, or a custom one where each registration function is
an HTTP method and the handler is a single positional argument — declare it:

```yaml
static:
  routers:
    - package: github.com/labstack/echo/v4   # where the router type is declared
      methods: [GET, POST, PUT, DELETE, PATCH, HEAD, OPTIONS]  # HTTP method = name uppercased
      # routeArg: 0  (default)   handlerArg: 1  (default)
```

gin's per-method router works the same way — its handler is variadic, and the
final handler in `r.GET(path, mw…, h)` is the one rooted. The one shape this
does **not** cover is gorilla/mux, where the method comes from a chained
`.Methods("GET")` call rather than the function name or route string.

---

# Part 2 — groundwork: judging the facts across the lifecycle

groundwork only ever **reads** flowmap's graph; it never produces it. Every
verdict is a pure function of `(graph, policy)`, byte-deterministic, and
three-valued — proven, violated-with-a-witness, or abstained-with-a-reason.
Silence is never a silent pass.

## 7. Author a baseline policy

Don't hand-write the policy from scratch, and don't assert invariants you haven't
measured — derive them from the real graph, then enforce only what holds today:

```sh
flowmap graph . > graph.json
groundwork init graph.json --name mysvc --guide policy-guide.md > policy.json
groundwork policy-check policy.json        # load + validate
```

`init` proposes layers from the package structure and, crucially, **allow-lists
every latent layering violation it finds** with a `"baseline at init — latent
violation, review"` reason. That encodes the load-bearing adoption rule: **fail
only on _new_ violations, never on five-year-old debt.** Walk the generated
allow-list with your team — each entry is a real "refactor or bless" decision —
and delete the ones you intend to fix. From then on the gate ratchets: the
baseline can only shrink.

The policy vocabulary (every family optional; unknown keys fail closed):

| Family | JSON key | Proves |
|---|---|---|
| Layering | `layers` + `layering` | no call skips a layer (handler→app→store); `allow[]` blesses exceptions |
| Reachability (negative) | `must_not_reach` | "an unauthenticated route never reaches the charge API" — over *all* paths |
| Waypoint | `must_pass_through` | "every entrypoint→DB path goes through the auth check" (`from`/`through`/`to`) |
| Concurrency | `no_concurrent_reach` | "no DB write from a goroutine" |
| I/O budget | `io_budget.max_writes_per_route` | a route's write surface stays bounded |
| Blind-spot ratchet | `blind_spot_ratchet` | dynamic dispatch can't silently erode the graph (`gate: true` to enforce) |
| Effect ratchet | `effect_ratchet` | no new external write target (table/topic/peer) base→branch without an `allow` entry (`gate: true` to enforce) |

Set `require_proof: true` on a reach/waypoint rule to turn "the static graph
found no path" into "and the path is fully resolvable" — it then fails closed
through a blind spot instead of passing vacuously.

**Domain lifecycle rules live in `.flowmap.yaml`, not the policy** (flowmap holds
the per-function CFG that proves them):

```yaml
obligations:
  - name: tx-must-close
    acquire: "example.com/mysvc/internal/store#BeginTx"
    release: ["example.com/mysvc/internal/store#Commit",
              "example.com/mysvc/internal/store#Rollback"]
  - name: audit-before-publish
    require: "example.com/mysvc/internal/audit#Write"
    before:  "example.com/mysvc/internal/eventbus#Publish"
```

A SATISFIED obligation is a universal proof no test suite can produce; a later
SATISFIED→VIOLATED flip surfaces in `review`/`verify` as a *new* violation.

## 8. The lifecycle loop

The same policy is consulted at all four seats, so quality doesn't depend on
which agent ran or how it was prompted:

| Phase | Command(s) | What it hands you | Oracle |
|---|---|---|---|
| **Design** | `ground`, `reach`, `triage --fail` | which rules bind the code you'll touch; blast radius; "what's irreversibly done if X faults" | engineer |
| **Build** | `ground` → edit → `verify` | new violations / scope creep / breaking contract, each naming the exact edge | the gate |
| **Review** | `review` + `verify-artifact` | three-valued verdict, shape, only newly-introduced findings, contract + I/O movement | human (intent) |
| **Triage** | `triage --route/--frame/--table/--event/--peer`, `flowmap behavior ingest` | bounded suspect set + partial-effect answers, from the alert's own words | responder |

### Design — constraints become requirements *before* generation

```sh
groundwork ground graph.json '<fqn>' --policy policy.json    # what binds this fn
groundwork reach  graph.json '<fqn>'                         # blast radius
groundwork triage --fail --peer credit-bureau graph.json     # what-if a peer faults
```

The `ground` card is derived with the **same matchers as the merge gate**, so it
never promises a guardrail that won't bind. A good plan includes its own *rule
changes* — a new `must_pass_through` or obligation — reviewed like code, encoding
the lesson at the feature's birth.

### Build — deterministic prevention replaces deterministic rejection

The loop is **ground → edit → verify**. Before pushing, self-check locally
against the same gate CI will run:

```sh
flowmap graph . > branch.graph.json
groundwork verify policy.json base.graph.json branch.graph.json
```

Every classic agent mistake fails with a witness: the layering skip names the
bypass path, the missing rollback names the leaking exit by line, "make it async"
trips `no_concurrent_reach`, reaching for reflection trips the blind-spot
ratchet. Iteration happens against a judge that consumes zero human attention.

### Review — a computed artifact, not author-written prose

```sh
groundwork review policy.json base.graph.json branch.graph.json   # BLOCK exits non-zero
groundwork diff   base.contract.json branch.contract.json        # boundary-contract movement
```

The verdict is three-valued and the silence is legible:

- **BLOCK** — a structural invariant or the contract broke (exits non-zero).
- **STRUCTURALLY-CLEAR** — shape preserved; explicitly *not* a logic sign-off.
- **NO-STRUCTURAL-SIGNAL** — body-only change; the graph abstains and says so
  (exits 0, but is *not* a clean bill of health — send full attention to logic).

Anyone — reviewer or CI — recomputes the digest to prove the artifact is
authentic (catches a tampered *or* re-signed-forgery artifact):

```sh
groundwork verify-artifact artifact.json policy.json base.graph.json branch.graph.json
```

### Triage — the alert's own words are the inputs

```sh
groundwork triage --route "POST /api/v1/loans/{id}" graph.json   # mount prefixes / IDs tolerated
groundwork triage --table loans --fail graph.json                # + the partial-effect answer
flowmap behavior ingest <incident.otlp.json> …                   # locate the divergence in-set
```

Measured on the dogfood fixture at 10/10 recall, median 8% hunt space
(`docs/groundwork/drills.md`). Causes outside the code (config, infra, data) are
out of scope, and every fault card says so.

## 9. Wire the structural gate into CI — and the one trust boundary

**The load-bearing condition:** the graphs MUST be generated by trusted CI from
the checked-out source, **never accepted from the author/agent under review.** An
agent that supplies its own branch graph forges a pass trivially by omitting the
offending edge — groundwork cannot detect that and does not try. flowmap
execution must sit inside the CI trust boundary; the agent may *read* graphs but
never *supply* the one it is judged against.

Add a `structural-gate` job alongside the currency/snapshot gates in
`.github/workflows/gates.yml`:

```yaml
  structural-gate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }            # need the base ref for the diff
      - uses: actions/setup-go@v5
        with: { go-version: "1.25.11" }     # pin: the graph is SSA-deterministic per toolchain
      - run: go install ./cmd/flowmap ./cmd/groundwork   # or a pinned release; producer + judge in lockstep
      # Branch graph from the code under review, stamped to its commit.
      - run: flowmap graph --stamp "$GITHUB_SHA" . > branch.graph.json
      # Base graph from the merge target — same trusted CI, never the PR.
      - run: |
          git worktree add /tmp/base "origin/${{ github.base_ref }}"
          flowmap graph /tmp/base > base.graph.json
      - name: pre-flight gate
        env: { GROUNDWORK_REQUIRE_STAMP: "1" }   # a forgotten --expect fails, never silently skips
        run: groundwork verify policy.json base.graph.json branch.graph.json --expect "$GITHUB_SHA"
      - name: reviewer artifact
        if: always()
        run: groundwork review policy.json base.graph.json branch.graph.json --expect "$GITHUB_SHA" | tee mr-review.txt
```

`--stamp`/`--expect` bind the verdict to the exact commit, so a stale graph can't
gate the wrong code; `GROUNDWORK_REQUIRE_STAMP=1` makes the binding mandatory.
Producer and judge are deployed in lockstep — the graph JSON is decoded strictly,
so unknown fields fail loudly — so pin both to the same release.

**Optional: arm the behavioral impeachment dimension.** Once you have committed
golden snapshots (Part 1, step 3), feed them to the same gate so *observed*
behavior can impeach a static negative the graph proved — a `must_not_reach`
`require_proof` proof the runtime contradicts is downgraded to CANT-PROVE and
BLOCKs, and a witnessed breach upgrades to a behaviorally-confirmed `VIOLATED`:

```yaml
      - name: pre-flight gate (with impeachment)
        env: { GROUNDWORK_REQUIRE_STAMP: "1" }
        run: groundwork verify policy.json base.graph.json branch.graph.json
             --expect "$GITHUB_SHA" --corpus flows/ --capture integration
```

It is **observe-first**: disclosed from day one, but it only *blocks* once you set
`impeachment_gate.gate` in the policy and a human ratifies — so you can watch what
it finds before it can ever fail a merge. `--capture` asserts the corpus's fidelity
grade (`production`/`integration`); a grade that contradicts what the corpus self-
describes fails closed, so a test corpus can never mint a gating impeachment. This
is a *counterexample finder, not an audit* — it finds unsoundness only on exercised
paths and never proves the graph correct.

## 10. Route the policy through CODEOWNERS

`policy.json` and the obligation rules in `.flowmap.yaml` are architectural truth:
a change to them is unbypassable and routes to a human, exactly like the gated
artifacts in Part 1.

```
**/policy.json        @your-team     # the invariants + the allow-list
**/.flowmap.yaml      @your-team     # classification + obligation rules
```

`groundwork exceptions policy.json graph.json` audits the allow-list and flags
entries that no longer excuse anything — so a blessed exception can't quietly rot
into a permanent blind spot.

## 11. Serve it to agents over MCP (optional, high-leverage)

`groundwork mcp` exposes triage/reach/ground/fitness/exceptions as MCP tools so a
coding agent runs the **ground → edit → verify** loop itself:

```sh
groundwork mcp graph.json --policy policy.json                       # stdio, one service
groundwork mcp graph.json --policy policy.json --corpus flows/       # + the audit-only `impeach` lens
groundwork mcp --service pay=pay.json --service ledger=ledger.json   # the neighborhood's maps
groundwork mcp pay.json --http 127.0.0.1:8137 --token "$T"           # team-shared (token required off loopback)
```

Adding `--corpus` enables the audit-only `impeach` lens: it discloses where
observed behavior has already contradicted the graph's "this can't happen," so the
agent is warned off a proven-absent claim at a real seam *before* it edits — it
**never gates** (the gate is `verify --corpus` in CI). The graph the server reads is
still CI-generated — the agent reads it, never writes it. Run with `--log
calls.jsonl` and read `groundwork transcript calls.jsonl` to see whether sessions
actually cite card facts.

## What groundwork does *not* verify — state this up front

A green structural verdict certifies exactly one proposition: *the change did not
alter the declared structural shape of the system.* It does **not** catch logic
bugs inside an unchanged shape, data/value correctness (the `<dynamic>` topic on
a publish), duplicated helpers / "reinventing the wheel" (no rule means no
signal), or sins of omission (a validation the agent should have added leaves no
edge to flag). Those stay with tests, types, and the human reviewer. The unique
value is the **anti-drift ratchet** and **all-paths safety invariants** — the
global properties a self-correcting agent loop and its own (correlated) tests
structurally cannot self-enforce.

---

## Scope (v1)

This recipe gates **in-process, single-service** flows per MR — fast,
deterministic, one clock domain. For an **existing out-of-process e2e suite**
(Dockerized services, an OTel collector, real network), flowmap also reads
captured OTLP traces post-hoc and maps the boundary effects a run exercised —
non-gated by default, opt-in to a no-new-effects gate per flow. See
[`integration/otlp-integration-guide.md`](./integration/otlp-integration-guide.md)
and the runnable `examples/posthoc-e2e/`. The boundary contract is still
exhaustive over statically-reachable paths; what a flow *doesn't* exercise is
exactly what `flowmap coverage` reports.
