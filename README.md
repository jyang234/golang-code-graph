# flowmap + groundwork

**Deterministic guardrails for AI-speed Go development.**

When code is written faster than humans can carefully read it — by coding
agents, or simply by a team moving quickly — the reviewer's question stops
being "is every line right?" and becomes "*what am I actually being handed,
and what still holds?*" This repo answers that question with **computed,
reproducible evidence instead of prose**: no AI sits in any verdict, every
output is a pure function of its inputs, and anything the tools cannot prove
is *disclosed* rather than silently passed.

Two cooperating tools, one interface between them:

```
   source code ──► flowmap ──►  graph.json            ─┐
   (a service)     (producer)   boundary-contract.json │──► groundwork ──► verdicts,
                                golden flow snapshots  │    (the judge)    cards, gates
                  policy.json (human-authored) ────────┘
```

- **flowmap** *produces* facts from a service: its static call graph with
  typed boundary effects (`go/packages → go/ssa → go/callgraph`), a gated
  inter-service **boundary contract**, per-function **path-obligation
  verdicts** and **partial-effect facts** computed from each function's
  control-flow graph, and **behavioral golden snapshots** captured from real
  OpenTelemetry traces.
- **groundwork** *judges* those facts against a human-authored policy:
  architectural fitness gates, computed MR review artifacts with an
  unfakeable digest, incident-triage cards, pre-edit grounding cards, and an
  MCP server that puts all of it in an agent's hands.

A human (via CODEOWNERS) is always the oracle. The tools never guess: every
claim is either **proven**, **violated with a witness**, or **abstained-with-
a-reason** — silence is never a silent pass.

## Why you want this

**If you review code (or merge an agent's code):** `groundwork review` hands
you a verdict computed *from the code's structure* — BLOCK, STRUCTURALLY-CLEAR,
or NO-STRUCTURAL-SIGNAL — with the exact new violations, contract changes, and
I/O effects the change introduces. The author cannot embellish it: the
artifact carries a digest any verifier recomputes from the trusted graphs.
The three-valued verdict is the point — "the graph has nothing to say" is
stated outright, never dressed up as "looks fine."

**If you own architecture:** the policy turns the rules in your head into
fail-closed CI checks — layering, "an unauthenticated path must never reach
the charge API" (`must_not_reach`), "every entrypoint-to-DB path goes through
the auth check" (`must_pass_through`, with a selector that automatically binds
brand-new handler packages), "no DB writes from goroutines"
(`no_concurrent_reach`), per-route write budgets, and a **blind-spot ratchet**
that stops dynamic dispatch from quietly eroding the graph everything else
depends on. Exceptions are first-class and *audited*: `groundwork exceptions`
flags allow-list entries that no longer excuse anything.

**If you own domain invariants no generic linter can know:** path obligations
prove lifecycle shapes over *every* CFG path — "after `BeginTx`, every path
commits or rolls back", "the audit write precedes the publish". A SATISFIED
verdict is a universal proof no test suite can produce; a VIOLATED names the
leaking exit; a CANT-PROVE tells you exactly why the claim would be unsound.
`staticcheck` knows `*sql.Rows`; only you know your event bus — these rules
are keyed to *your* named functions.

**If you carry a pager:** `groundwork triage` is the incident front door.
Hand it a symptom — the failing route straight off the alert
(`--route "POST /api/v1/loans/{id}"`, mount prefixes and concrete IDs
tolerated), a stack frame in runtime form, a corrupted table, a missing event,
a slow peer — and get the bounded suspect set, the implicated routes, and (with
`--fail`) the partial-effect answer responders are desperate for: *"if the
charge call faulted, `loan.approved` was already published — you may have
approved-but-uncharged loans."* Then `flowmap behavior ingest` takes the
incident's own OTel trace and pinpoints where it diverged from known-good.
The graph narrows; the telemetry locates. No test cases authored, ever. And these claims are *measured*, not asserted:
the committed effectiveness drills hold triage at 10/10 recall with a median
hunt space of 8% of the graph — as test assertions, so the numbers re-verify
on every run ([`docs/groundwork/drills.md`](docs/groundwork/drills.md)).

**If you build with coding agents:** `groundwork mcp` serves all of it as MCP
tools, and `ground` closes the loop *before* the edit: the grounding card
tells the agent (or you) exactly which rules bind the function it is about to
touch — derived with the same matchers the gates use, so the card never
promises a guardrail that doesn't bind. The edit loop becomes
**ground → edit → verify**, one rule set at both ends; deterministic
prevention is cheaper than deterministic rejection.

## Five-minute tour

```console
# Produce the facts (flowmap, the trusted side):
$ flowmap graph svc/ > graph.json          # call graph + boundary effects + obligations
$ flowmap boundary svc/                    # gated inter-service contract

# Explore and gate (groundwork, the judge):
$ groundwork reach graph.json '(*svc/internal/store.Store).Tx'   # blast radius of one function
$ groundwork fitness policy.json graph.json                      # do the invariants hold?
$ groundwork verify policy.json base.json branch.json            # fail-closed pre-merge gate
$ groundwork review policy.json base.json branch.json            # the reviewer's computed artifact

# During an incident:
$ groundwork triage --route "POST /loans" graph.json             # the alert's own words
$ groundwork triage --table users graph.json                     # who touches it, what's implicated
$ groundwork triage --fail --peer credit-bureau graph.json       # what-if: peer down

# Before an edit (human or agent):
$ groundwork ground graph.json '<fqn>' --policy policy.json      # what binds this function
$ groundwork mcp graph.json --policy policy.json                 # the same lenses, as MCP tools
```

Every command is read-only over CI-generated artifacts, byte-deterministic,
and honest about its limits — see the blind-spot and `<dynamic>` disclosures
threaded through every output.

## What's where

| You want to… | Read |
|---|---|
| Understand the concepts and every groundwork surface | [`docs/groundwork/usage.md`](docs/groundwork/usage.md) — the practical guide, primer included |
| See how it changes the day-to-day — responder, developer, reviewer, each before/after with Claude Code | [`docs/groundwork/personas.md`](docs/groundwork/personas.md) |
| Wire flowmap into a service (OTel → contract → flow tests → CI) | [`docs/adopting-flowmap.md`](docs/adopting-flowmap.md) |
| Understand *why* it is built this way (thesis, failure modes, pressure tests) | [`docs/groundwork/README.md`](docs/groundwork/README.md) and [`docs/groundwork/distilled-learnings.md`](docs/groundwork/distilled-learnings.md) |
| See the component specifications (the source of truth) | `docs/*.md` (tier map, static extractor, canonicalization, capture harness, golden diff) |
| Follow the feature plans and their validation criteria | `docs/design/*-plan.md` |
| See how well it actually works — and what is honestly unproven | [`docs/groundwork/drills.md`](docs/groundwork/drills.md) (measured) and [`docs/groundwork/scorecard.md`](docs/groundwork/scorecard.md) (graded by evidence class) |
| A complete worked example service | `testdata/fixtures/loansvc` (and `testdata/groundwork/{layeredsvc,blindsvc,obligsvc}`) |

## Layout

```
cmd/flowmap/     CLI: boundary [--check] | graph [--entry] [--stamp] | diff | coverage | behavior ingest | version
cmd/groundwork/  CLI: reach | triage | ground | fitness | review | verify | diff | verify-artifact
                      | exceptions | policy-check | mcp | version
harness/ capture/ flow/ ir/   PUBLIC: in-process flow-test capture + the canonical IR
internal/        the engines (static/, canon/, groundwork/, …)
testdata/        hermetic fixture services (own modules) + committed goldens
```

## The gates (and how to keep them green)

Distinct gate mechanisms, unified by CODEOWNERS routing and the
human-as-oracle:

- **Currency gate (static).** The boundary contract is a pure function of the
  code, so CI regenerates it and fails on drift:
  `flowmap boundary --check <service-dir>`. After a boundary change, rerun
  `flowmap boundary` and commit the updated contract alongside the code.
- **Snapshot-assertion gate (behavioral).** Golden flow snapshots ride
  `go test`; after an intended behavior change, rebase with `-update` and
  commit.
- **Structural gate (groundwork).** CI regenerates base and branch graphs from
  checked-out source and runs `groundwork verify` — new violations, scope
  creep, and breaking contract changes fail closed. The graphs MUST come from
  trusted CI, never from the author under review; that is the load-bearing
  trust boundary (see [`docs/groundwork/usage.md`](docs/groundwork/usage.md)).

CODEOWNERS routes the gated artifacts, `.flowmap.yaml`, and `policy.json` to a
human reviewer — a contract, golden, rule, or allow-list change is
unbypassable.

## Coverage

`flowmap coverage --flows <goldens-dir> <service-dir>` reports the boundary
effects no committed flow exercises — the delta between what the code *can*
do and what the tests *prove* it does. Informational, not a gate: a gap means
"write a flow for this."

## Schema versioning & regeneration

Gated artifacts carry a schema version (e.g. `flowmap.boundary/v1`). When a
canonical form changes, the version bumps and all adopting services regenerate
the affected artifacts in a coordinated change — the real blast radius, made
explicit rather than silent. The graph JSON is decoded strictly on the
groundwork side (unknown fields fail loudly), so producer and judge are
deployed in lockstep by design.

## Develop

```
make verify    # build + vet + lint + test + fixture gate + gofmt (the per-phase gate)
```
