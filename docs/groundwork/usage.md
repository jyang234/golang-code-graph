# groundwork — usage

> **`ACTIVE`** · usage guide · _reviewed 2026-06-13_

`groundwork` is a deterministic verification layer over a Go service's static
call graph. It turns architectural facts that normally live in someone's head —
"the handler layer must not touch storage directly", "this route may do at most
two writes", "an unauthenticated path must never reach the charge API" — into
**computed, fail-closed checks** that run on every change. No AI sits in any
verdict; every output is a pure function of `(policy, graph, delta)`, so the same
inputs always produce the same answer, digest included.

This page is the practical guide: how it fits with flowmap, the commands, and a
worked end-to-end review. For the *why* (the thesis, failure modes, and the
adversarial pressure-testing behind the design), see the
[design record](README.md).

---

## The concepts in five minutes

If you are new to the toolset, these seven ideas are everything the rest of
this page builds on.

**The call graph.** flowmap compiles your service the same way the Go
toolchain does, then records *which function can call which* — every node is
one of your functions, every edge one possible call. This is the map of what
your code **can** do, computed from the code itself, with no tests run and no
instrumentation added.

**Boundary effects.** Where your code touches the outside world, the edge is
typed: `boundary:db UPDATE users`, `boundary:bus PUBLISH loan.approved`,
`boundary:credit-bureau GET /score/{id}`. Reachability plus boundary effects
is what turns "this function changed" into "these routes can now write that
table" — the question a reviewer actually has.

**Entrypoints and reachability.** Functions nobody calls are where requests
enter: HTTP handlers, bus consumers, `main`. Walking edges *backward* from a
function tells you which entrypoints it is live behind (its blast radius);
walking *forward* tells you everything it can touch. Most groundwork surfaces
are compositions of these two walks.

**Tiers.** Every node and edge carries a salience tier (1 = most consequential
— publishes, writes, inbound routes; 4 = noise). Tiers keep cards and
snapshots focused on what matters.

**Blind spots, disclosed.** Static analysis has limits — reflection, `unsafe`,
dynamically-named topics, high-fan-out dispatch. flowmap *records* every such
limit in the graph (`blind_spots[]`, `<dynamic>` markers), and every
groundwork claim that crosses one says so. This is the honesty discipline:
you always know where the map stops being trustworthy.

**Three-valued verdicts.** Nothing here answers only pass/fail. Checks answer
**proven** / **violated (with a witness)** / **cannot-prove (with the
reason)**, and reviews answer BLOCK / STRUCTURALLY-CLEAR /
NO-STRUCTURAL-SIGNAL. The third value is the load-bearing one: a tool that
cannot prove something *says so* instead of passing silently — so green means
proven, never just "nothing noticed."

**Determinism.** Every output is a pure function of its inputs — same graph,
same policy, same answer, byte-for-byte, on any machine. That is what makes a
digest meaningful, a gate reproducible, and a disagreement debuggable. It is
also the acceptance bar for every feature in this toolset: anything heuristic
or sampled lives outside the verdict path, by design.

Every other term used in this guide — substrate, frontier, waypoint, obligation,
reclaimer, hunt space, and the rest — is defined in the [Glossary](#glossary) at
the end.

---

## How groundwork and flowmap fit together

flowmap and groundwork are **two separate programs with one interface** — the
graph JSON between them:

```
   source code ──► flowmap ──►  graph.json          ─┐
   (a service)     (producer)   boundary-contract.json│──► groundwork ──► verdict
                                                       │   (the judge)     (+ digest)
                  policy.json (human-authored) ────────┘
```

- **flowmap is the producer.** `flowmap graph <service>` builds the call graph
  (`go/packages → go/ssa → go/callgraph`) and emits it as canonical JSON — nodes
  (`fqn`, `sig`, `tier`, `fallible`), edges (caller→callee, with `boundary` and
  `concurrent` flags), a blind-spot manifest, and the level-2 disclosure
  sections computed from each function's CFG and the discovered roots:
  `obligations` (path-obligation verdicts), `effect_order` (partial-effect
  facts), and `entrypoints` (the route/topic → handler join). An optional
  `--stamp <sha>` records a caller-supplied identity for `--expect`
  verification. `flowmap boundary <service>` emits the gated inter-service
  **boundary contract** (routes, published/consumed events, external
  dependencies).
- **groundwork is the judge.** It only ever *reads* those JSON files; it never
  runs flowmap. Keeping the producer and the judge as different binaries is what
  lets CI control which runs where — the security boundary is around graph
  *generation*, not around groundwork (see [the trust boundary](#the-trust-boundary)).

groundwork consumes three things:

| Input | Produced by | Role |
|---|---|---|
| `graph.json` | `flowmap graph` | the structural substrate (what calls what, what touches the outside world) |
| `boundary-contract.json` | `flowmap boundary` | the inter-service surface, for `diff` |
| `policy.json` | a human (CODEOWNERS-gated) | the declared architecture to enforce |

### The call-graph substrate (`--algo`) and the proof stance

`flowmap graph` builds the call graph with one of three algorithms, chosen with
`--algo`:

| `--algo` | What it is | When |
|---|---|---|
| `rta` (default) | Rapid Type Analysis from the discovered roots | the sweet spot for services — precise enough, cheap in CI |
| `vta` | Variable Type Analysis, **RTA-seeded** | refines interface-dense dispatch by type-flow — far fewer spurious callers/callees |
| `cha` | Class Hierarchy Analysis, rootless | fallback for a library with no resolvable entry points |

**All three are sound over-approximations** — modulo the same reflection/unsafe
frontier, which is already disclosed in the `blind_spots` manifest, not silently
dropped. VTA is seeded from RTA and *refines* dynamic-dispatch edges by type
flow; it does not drop real edges. So a `must_not_reach` `PROVEN-ABSENT` (or a
`must_pass_through` `SATISFIED`) is **valid on any of the three** — VTA is a
*blessed* proof substrate, not exploration-only, and it is strictly tighter than
RTA (it removes the spurious `HighFanOut` paths that inflate RTA's reach,
yielding fewer spurious `REACHABLE` violations and fewer cautions). RTA stays the
default for cost; reach for `--algo vta` when interface-DI dispatch makes RTA's
fan-out noisy, including for absence proofs.

The one caveat to keep stating is unchanged by algorithm: soundness is **modulo
reflection/unsafe** for all three. Adopting VTA does not widen that hole; it only
changes precision.

**Provenance.** The graph records which algorithm built it (`algo`) and its notes
(`caveats`), and `fitness`, `review`, and `verify` echo them on a `substrate:`
line — so a gated verdict self-certifies the substrate it was computed on
("`substrate: vta — vta refined over rta from N discovered root(s)`") rather than
leaving a reviewer to guess. When a `review`/`verify` base and branch were built
on *different* algorithms, the mismatch is disclosed as a caveat (a delta across
substrates can move for the analyzer's reasons, not the code's). A graph from a
pre-provenance flowmap reads `substrate: unrecorded`.

The graph also records which flowmap build *produced* it (the `tool` field —
where `--stamp` is the caller-supplied identity of the *code*, `tool` is the
identity of the *producer*). "Same code → same graph" holds only within one tool
version, so when a `review`/`verify` base and branch were produced by *different*
flowmap builds, the producer mismatch is disclosed on the same line — a diff may
then be a tool artifact (a relabeled effect, an SSA-order shift), not a code
change. Committed goldens are generated `tool`-free, the same convention as
`--stamp`, so they stay byte-identical across producing builds.

A graph built with `flowmap graph --reclaim` carries edges recovered at a
framework dispatch seam (the oapi strict-server `wrapper→$1` hop), each tagged with
the reclaimer in a `via` field. groundwork consumes that field and adds a
`reclaim-informed: N via <reclaimer>` clause to the same substrate line, so a
verdict that leaned on a reclaimed edge is auditable as such. Reclaimers only add
sound edges, so a proof of *absence* over a reclaimed graph is at least as strong;
the disclosure exists so a *reachable* verdict resting on a reclaimed edge is not
mistaken for one the base graph already saw.

`init` also records the algorithm it proposed against in the policy's `substrate`
field, and `fitness`/`verify` flag a **policy-vs-graph** mismatch the same way: a
policy proposed on `vta` but checked against an `rta` graph (the `flowmap graph`
default) appends a substrate-mismatch note to the same provenance line, because
`rta` over-approximates dynamic dispatch and can surface reachability findings
`vta` ruled out — so the spurious findings read as an analyzer artifact, not a
regression. It is a **disclosure, never a gate finding** (a finding would leak
into `review`/`verify`'s base-vs-branch diff and mis-flip a verdict), so it
discloses without blocking. The remedy it names: build the gate graph with the
same `--algo`, or re-init the policy.

---

## The policy

The policy is the single, human-authored source of architectural truth. It is a
gated artifact — if the agent under review could author it, it would grade its
own homework — so it is declarative and validated strictly on load.

```json
{
  "service": "layeredsvc",
  "version": 1,
  "substrate": "vta",
  "layers": [
    {"name": "handler", "packages": ["example.com/layeredsvc/internal/handler"]},
    {"name": "app",     "packages": ["example.com/layeredsvc/internal/app"]},
    {"name": "store",   "packages": ["example.com/layeredsvc/internal/store"]}
  ],
  "layering": {"roots": ["example.com/layeredsvc"], "allow": []},
  "must_not_reach": [
    {"name": "read-route-stays-read-only",
     "from": ["(*example.com/layeredsvc/internal/handler.Server).GetUser"],
     "to":   ["boundary:db INSERT", "boundary:db UPDATE", "boundary:db DELETE"]}
  ],
  "must_pass_through": [
    {"name": "app-guards-db",
     "from": ["entrypoint:*"],
     "to":   ["boundary:db"],
     "through": ["(*example.com/layeredsvc/internal/app.Service)"],
     "allow": [{"from": "example.com/layeredsvc.main", "reason": "composition root"}]}
  ],
  "io_budget": {"max_writes_per_route": 2},
  "blind_spot_ratchet": {
    "gate": false,
    "allow": [{"kind": "reflect", "site": "example.com/layeredsvc/internal/codec", "reason": "audited decoder"}]
  }
}
```

It declares seven invariant families:

- **`layers`** — ordered top→bottom; a call may stay within a layer or descend
  one, never skip a layer or call upward. `roots` exempts the composition root
  (main); `allow` is the reviewed-once exception list. The check judges *effective*
  edges, so a skip smuggled through an unassigned helper package
  (`handler → codec → store`) is caught, while the legitimate `handler→app→store`
  spine is not.
- **`must_not_reach`** — negative reachability invariants (the all-paths safety
  class). Add `"require_proof": true` to a high-stakes rule to make it fail closed
  when the graph cannot prove absence.
- **`must_pass_through`** — waypoint invariants: every path from a `from`
  source to a `to` target must pass through a `through` function ("every
  entrypoint-to-DB path goes through the auth check"). `from` supports
  `entrypoint:*`, which matches every graph source — deliberately, so a
  brand-new handler package cannot silently escape the rule; exempt the
  composition root via `allow`, not by narrowing `from`. A violation names the
  bypassing (source, target) pair and shows one shortest bypass path; every
  bypassing pair is reported, so a second bypass added on a branch surfaces as
  a *new* violation. Three-valued like `must_not_reach`: "no bypass over a
  blind frontier" is a caution, escalated by `require_proof`.
- **`no_concurrent_reach`** — no target matching `to` may be reached along a
  path entered via a concurrent edge (a go/defer call site): "no DB writes
  from goroutines" — the agent pattern of "make it async" introducing
  unsupervised effects. Same three-valued discipline and `require_proof`
  escalation. Disclosed v1 limit: the concurrent flag conflates `go` and
  `defer` sites.
- **`io_budget`** — caps external *writes* reachable from a route (the
  side-effect-blowout guard); reads don't count, and the composition root is
  exempt.
- **`blind_spot_ratchet`** — the drift ratchet on the graph's *own* soundness:
  no new blind spots base→branch without a reviewed `allow` entry. Every other
  check is only as good as the substrate, so unchecked growth in dynamic
  dispatch erodes them all silently. `review` always reports new blind spots
  (even with no ratchet configured); `gate: true` additionally makes them
  block `verify` — observe first, gate once the baseline is clean. The ratchet
  is one-directional: pre-existing and removed blind spots never fire it.
- **`effect_ratchet`** — the drift ratchet on the external *write surface*: no
  new boundary write target (a new table written, a new outbound POST, a new
  publish) base→branch without a reviewed `allow` entry. Same lifecycle as the
  blind-spot ratchet: always reported, `gate: true` to block. It sees new
  effect *labels*, not new uses of an existing label (the per-route delta
  section covers that); its soundness against laundering a write through
  dynamic dispatch leans on `blind_spot_ratchet` catching the new dispatch
  site — gate them together.

Beyond the invariant families, a policy may carry a **`brokers`** declaration —
not a gate but a *printed guarantee*. Keyed by bus name, it states the broker
semantics no static analysis can establish (`transport`, `delivery`, `ordered`,
`consumers`, `dedup`), which the CX-5 `chains` surface prints verbatim as the
**assumed** link of a cross-service chain (D-CX5: declared, never inferred). The
closed-vocabulary fields are validated on load, so a typo'd value cannot read as
a real guarantee. `signed_by` is the human warrant — an unsigned block prints
its values flagged UNSIGNED. See `docs/design/cx5-chains-surface.md`.

The cross-service lenses (`chains`, `fleet-events`) join publishers to consumers
**by static event name**, so they are **inert on `<dynamic>` messaging**: a fleet
whose topics are chosen at runtime publishes/consumes `boundary:bus … <dynamic>`,
which the name-join cannot bind. Rather than read as "this fleet does no
messaging," both lenses now say so explicitly and disclose the per-service count
of dynamically-named bus effects they could not name. The behavioral pipeline
resolves those runtime names (it observes the real topics); static analysis
cannot. Treat an empty chain/fleet-events on a `<dynamic>`-heavy fleet as "the
static half is blind here," not "there is nothing to see."

```json
"brokers": {
  "bus": {
    "transport": "sns->sqs (standard queue)",
    "delivery":  "at-least-once",
    "ordered":   "false",
    "consumers": "idempotent",
    "dedup":     "inbox UNIQUE(source_id, topic)",
    "signed_by": "John, 2026-06-13"
  }
}
```

#### The sensitive-flow rule pack (correctness plan, CX-4)

The data-safety bug classes — PII reaching a log sink, untrusted input
reaching raw SQL unsanitized — need **no new rule family**: they are
`must_not_reach` / `must_pass_through` instances with a curated vocabulary.
Declared exactly like any other rule:

```json
"must_not_reach": [
  {"name": "pii-never-logged",
   "from": ["example.com/svc/internal/profile.LoadProfile",
            "(*example.com/svc/internal/dispatch.Dispatcher).Deliver"],
   "to":   ["example.com/svc/internal/log.Write"],
   "require_proof": true}
],
"must_pass_through": [
  {"name": "untrusted-input-sanitized",
   "from": ["entrypoint:*"],
   "to":   ["boundary:db"],
   "through": ["example.com/svc/internal/sanitize.Clean"]}
]
```

`from` names the PII-handling functions (human-curated — only you know where
PII lives); `to` names the sinks. **Name functions and methods explicitly,
in their FQN forms** (a method's FQN is receiver-qualified:
`(*pkg.Type).Method`): rule patterns match by FQN prefix, so a bare package
pattern binds the package's functions but *not* its methods — a partially
bound PII rule is a guardrail you believe in that isn't fully there. After
declaring, run `groundwork ground` on a function you expect the rule to
cover and confirm it appears under *binding rules* — the card uses exactly
the checks' matcher. The semantics are exactly the family's, stated here so
the rule never claims more than it proves:

- **A pass is the strong direction**: no call path from PII-handling code to
  a sink exists, proven over all paths, modulo the disclosed blind spots —
  the class testing covers worst. `require_proof` makes blindness fail
  closed.
- **A violation is a lead, not a proven flow**: a call path *can* carry the
  data; whether it does on a feasible path is the reviewer's call, with the
  same allow-list discipline as layering.
- **The claim is call-reachability, not data flow**: the rule covers sinks
  reachable *from* PII-handling code. PII returned up the stack and logged by
  a distant caller is invisible to it — that caller belongs in `from` if it
  handles PII. Argument-level taint is deliberately out of scope (it is value
  semantics; see the correctness plan's D-CX4).

**Shipping preconditions — checkable on the graph before the rule lands.**
This pack is precise on clean graphs and pure noise on the wrong ones, and a
noisy rule spends trust the whole framework runs on:

- the graph's blind-spot count is low (a 0-blind-spot service makes the pass
  a real proof; behind 100+ HighFanOut sites it mostly cautions);
- PII handling is concentrated (a few packages/files to name in `from`);
- the sink list is bounded. On a dense graph with no PII and dozens of log
  sinks, the rule fires everywhere and proves nothing — **do not ship it
  there**.

No sanitizer functions yet? Ship the waypoint-less `must_not_reach` form
first; introducing named scrubbers later *upgrades* the rule to
`must_pass_through` — an adopter refactor, never a heuristic stand-in.

Rules whose `from` *or* `to` binds nothing in the graph (a typo'd FQN, a
package renamed out from under the pattern, or — the field's lesson — a
third-party sink whose methods are not graph nodes) are disclosed as a
**caution** — "from binds nothing — inert rule" / "to binds nothing — name a
first-party sink it can bind, or this invariant is vacuous" — instead of
passing silently; `require_proof` escalates either to a violation, since a
rule that cannot be evaluated guards nothing. (An earlier design treated an
empty `to` as the success state — "the forbidden thing does not exist." A
real run disproved it: the graph cannot tell that from "the sink is
unnameable", so a rule pointed at a third-party logger reported HOLDS while
the unsafe call sat one line away. A `to` that *does* bind somewhere but is
simply unreached stays a real proven-absent pass — only a wholly unbindable
target cautions.) This is why the sensitive-flow pack insists on a
**first-party** sink: `applog.Error` is a node a `to` can bind;
`zap.Logger.Error` is not.

Validate one with `policy-check`:

```console
$ groundwork policy-check policy.json
policy for "layeredsvc" (v1) — valid
  layers (top→bottom): handler → app → store
  layering: 0 allow-listed exception(s), 1 root package(s)
  must_not_reach: 1 rule(s)
  must_pass_through: 1 rule(s)
  io_budget: max 2 write(s) per route
  blind_spot_ratchet: observe-only, 1 allow-listed exception(s)
```

---

## The surfaces

```
groundwork reach <graph> <fqn>                          explore one function's blast radius
groundwork triage (--frame|--route|--table|--event|--peer) <v> [--fail] [--expect <stamp>] [--json] <graph>   incident triage card
groundwork ground <graph> <fqn> [--policy …] [--json]   pre-edit grounding card: what binds this function
groundwork exceptions <policy> <graph> [--json]         audit allow-lists; flag dead entries
groundwork init <graph> [--name …] [--out …] [--guide …]   propose a baseline policy from measured facts
groundwork mcp <graph> [--policy …] [--corpus <dir>] [--capture <grade>] [--expect …] [--log …]  serve the lenses (incl. audit-only impeach) as MCP tools over stdio
groundwork mcp --service <name>=<graph> …               one server, several services' maps (+ fleet-events, chains)
groundwork mcp … --http <addr> [--token <secret>]       team-shared streamable-HTTP transport
groundwork chains <graph>… [--service <name>=<graph>]… [--policy <p>]…   cross-service effect chains (CX-5, observational)
groundwork transcript <calls.jsonl> [--json]            summarize an mcp --log transcript (the E4 reader)
groundwork fitness <policy> <graph> [--expect <sha>] [--sarif]   evaluate invariants against one graph
groundwork review <policy> <base> <branch> [--expect <sha>] [--json]     computed MR review artifact
groundwork verify <policy> <base> <branch> [--scope …] [--expect <sha>] [--corpus <dir> [--capture production|integration]] [--json]  fail-closed pre-flight gate (--corpus arms the impeachment gate)
groundwork diff <base-contract> <branch-contract>       inter-service contract diff
groundwork verify-artifact <artifact> <policy> <base> <branch> [--expect <sha>]   prove an artifact authentic
groundwork policy-check <policy>                         validate a policy
```

### `reach` — explore a function

Bidirectional reachability for one function: who breaks if it changes (callers),
what it depends on (callees), which entrypoints it is live behind, the external
effects it reaches, and any blind spots on it. The blast-radius/grounding lens an
agent reads *before* editing.

```console
$ groundwork reach graph.json '(*…/handler.Server).Publish'
…
reachable external effects: 1
  bus PUBLISH <dynamic> [outbound-async]  ⚠ unresolved (soundness frontier)
```

The `⚠` marks where the graph runs out — a `<dynamic>` effect it can't name
statically. groundwork is exact about structure *and* explicit about where
structure stops.

### `fitness` — evaluate invariants against one graph

Runs the policy's invariants over a single graph. Violations fail the gate
(non-zero exit); cautions are surfaced but don't fail — the legible form of the
graph abstaining where it cannot prove a negative.

```console
$ groundwork fitness policy.json graph.json
substrate: rta — rta from 3 discovered root(s)
fitness OK — 3 invariant(s) hold, 0 caution(s)
```

The `substrate:` line is provenance: a green pass is only as strong as the
call-graph algorithm it was computed on, so the verdict states which one ran (see
[the call-graph substrate](#the-call-graph-substrate---algo-and-the-proof-stance)).

`must_not_reach` is three-valued: `PROVEN-ABSENT` (silent pass), `NO-PATH-FOUND`
(a caution naming where the graph went blind, e.g. `reflect at encode.Marshal`),
and `REACHABLE` (a violation). A silently-blind "no path" is never disguised as a
proof.

### `review` — the computed MR artifact

Compares a base graph to a branch graph and computes the review a human reviewer
needs — *from the code's structure, not the author's prose*. The verdict is
three-valued, so green never means more than it should:

```console
$ groundwork review policy.json base.json branch.json
# MR structural review — BLOCK
digest adb67c06a1f6c8e6… · recompute to verify (deterministic; not author-editable)
substrate: rta — rta from 3 discovered root(s)

Shape: cross-package
Touches: handler(+1)

⛔ Introduces 1 invariant violation(s)
- layering — handler → store skips 1 layer(s)
  - (*…/handler.Server).GetUserFast → (*…/store.Store).SelectUser
```

(`GetUserFast` is a new exported handler method, but it is not bound to a route —
it is a graph *root* without being an external *entrypoint* — so it is reported in
the structural delta above, not as an external-contract change. Only a removed or
added HTTP route / consumed topic moves the contract section; internal
orphan-root and closure churn does not.)

- **`BLOCK`** — a new invariant violation or a breaking contract change.
- **`STRUCTURALLY-CLEAR`** — the shape changed but no invariant broke. *Not* a
  logic or test sign-off.
- **`NO-STRUCTURAL-SIGNAL`** — the graphs are identical (a body-only change); the
  graph abstains and says so — exactly where logic review matters most.

The same feature wired correctly (`handler→app→store`) renders
`STRUCTURALLY-CLEAR`; wired to skip the layer it renders `BLOCK` naming the exact
edge — *same description, different computed verdict.* The artifact also reports
shape, touched packages, contract movement (additive vs breaking), I/O effect
changes, and which existing entrypoints the change is now live behind. Add
`--json` to emit the canonical artifact for archival or `verify-artifact`.

The **contract** section is the inter-service surface: named external
entrypoints (HTTP routes and consumed topics, from the `entrypoints` join) plus
bus publishes/consumes and outbound dependencies. Removing one is breaking. It is
deliberately keyed on *named entrypoints*, not on every graph root: an unwired
exported method, a closure renumbered by a refactor, or an internal function left
rootless by a deleted backend are all roots but none are inter-service contract,
so they surface in the structural delta, not as a (breaking) contract change. The
trade-off is services-scoped — for a graph that exposes no routes/topics (or whose
routes flowmap did not resolve into the entrypoints join) a removed root is not
flagged here; such a removal shows in the node/edge delta instead.

Two sections attribute write-surface movement beyond the global effect diff:

- **`route_io_deltas`** — every route whose distinct-write-target set changed,
  with both sides' counts and the targets added/removed. The load-bearing case
  is the **lost write**: a route that reached `db INSERT audit_log` in base
  and no longer does, while the global effect set is unchanged because another
  route still writes it — invisible to every other section. Counts are static
  write *targets*, not runtime volume; a side counted over a blind frontier is
  marked, since a delta against a blind side may be the graph's knowledge
  shifting rather than the code's behavior. Disclosure only — it never gates.
- **`new_write_targets`** — the effect ratchet's findings: write labels new to
  the whole graph and not allow-listed. Reported always; blocking under
  `effect_ratchet.gate: true`.

Two caution sections keep a green verdict from over-claiming — both non-blocking,
surfaced at the gate, exit 0:

- **`new_cautions`** — a "the graph cannot prove this" disclosure *introduced by
  this MR* (e.g. a new route whose write budget the labeler can't read).
- **`standing_cautions`** — the same kind of disclosure that holds **identically
  on base and branch**, which the base→branch delta would otherwise suppress
  forever. The load-bearing case is an `io_budget` that is *unenforceable in
  steady state*: when storage builds SQL from non-constant strings (labeled `db
  call`), a within-budget pass is not a proof the write surface is bounded, and
  that caution stands unchanged across the edit. It is surfaced absolutely — like
  the `rule_liveness` audit — so the gate the agent actually converges against is
  never silent about a standing enforceability gap, not only the interactive
  `fitness` lens.

### `verify` — the fail-closed pre-flight gate

The gate form of `review`. It blocks a merge on a new violation, on a **breaking
contract change**, or on **scope creep** — a touched package outside the declared
`--scope`:

```console
$ groundwork verify policy.json base.json branch.json \
      --scope example.com/layeredsvc/internal/handler
# Pre-flight gate — BLOCK
digest 09af7826d3687a66… · recompute to verify (deterministic; not author-editable)
substrate: rta — rta from 3 discovered root(s)

🚧 1 package(s) outside the declared scope
- example.com/layeredsvc/internal/app
```

Scope is computed from the same base↔branch delta the review uses, so a change
that edits `handler` but wires a new edge into `app` is caught even when `app`
gained no node. An empty `--scope` disables only the scope check.

### `diff` — the boundary-contract diff

Compares two `flowmap boundary` contracts and flags inter-service surface
movement. Removing a route, a published event, or a consumed event is breaking
(a downstream service can break); additions and outbound-dependency changes are
informational.

```console
$ groundwork diff base-contract.json branch-contract.json
+ dependency audit-svc
+ route GET /healthz
- route PUT /users/{id}  ⚠ BREAKING
```

### `verify-artifact` — prove an artifact authentic

Recomputes a saved review artifact from the source graphs and reports
`AUTHENTIC`, `TAMPERED` (a field was edited without re-signing), or `STALE` (the
digest doesn't match what the real graphs produce — different code, or a re-signed
forgery). The digest alone proves nothing; the **recomputation from trusted
graphs** is the anchor.

```console
$ groundwork verify-artifact artifact.json policy.json base.json branch.json
AUTHENTIC — digest ee405bceee1a1949… matches the recomputation from the source graphs
```

---

## End-to-end: reviewing a change

Today the pipeline is run explicitly (the zero-touch CI wiring is deferred — see
below). Given a base commit and a branch:

```bash
# 1. Generate the two graphs with flowmap (the producer).
flowmap graph ./checkout-base   > base.graph.json
flowmap graph ./checkout-branch > branch.graph.json

# 2. Gate the change (fail closed).
groundwork verify policy.json base.graph.json branch.graph.json --scope <intended-packages>

# 3. Produce the reviewer's artifact.
groundwork review policy.json base.graph.json branch.graph.json

# 4. If the inter-service contract moved, diff it.
flowmap boundary ./checkout-base   && cp …/boundary-contract.json base.contract.json
flowmap boundary ./checkout-branch && cp …/boundary-contract.json branch.contract.json
groundwork diff base.contract.json branch.contract.json
```

Each step exits non-zero on a blocking finding, so the sequence backs a gate as-is.

The committed fixtures under `testdata/groundwork` are a complete worked example:
`layeredsvc` (a strict `handler→app→store` service), its `branch-good` and
`branch-skip` graph goldens, a sample `policy`, and base/branch contract goldens.
`testdata/groundwork/regen.sh` regenerates them all.

---

## Determinism and the digest

Every artifact carries `digest = sha256(canonical structural content)`. Because
the verdict is a pure function of `(policy, graph, delta)`, the digest is
reproducible: a reviewer or CI can recompute it and get the same value every time,
for every agent, regardless of how the MR was written. That is what lets a green
verdict be a hard gate an agent can converge against — and what an LLM-judge
cannot offer.

The digest is **not** a signature. It catches accidental edits and staleness;
unforgeability comes from a trusted party *recomputing* the artifact from the
source graphs (see `verify-artifact`).

---

## The trust boundary

Every guarantee above is conditional on one thing: **the graph must be generated
by a trusted party from the actual code.** An agent permitted to supply its own
branch graph forges any verdict by omitting the offending edge — and groundwork
cannot tell, because it faithfully judges whatever graph it is handed.

- **Run by a cooperating engineer (today):** generate the graphs yourself and run
  the commands above. The verdicts are real and useful — a local pre-flight gate,
  a drift ratchet, and an explainability artifact.
- **Gating agent-authored MRs unforgeably (deferred — "Phase 4"):** flowmap must
  run inside a CODEOWNERS-gated CI job that regenerates *both* base and branch
  graphs from checked-out source with a pinned toolchain, and the agent may *read*
  graphs but never *supply* the one it is judged against. That job is the trust
  anchor; it is intentionally not yet built. Until it exists, groundwork is a
  sound advisory tool, not an adversary-resistant gate.

See [`pressure-test.md`](pressure-test.md) for the adversarial analysis that
established this, and [`implementation-plan.md`](implementation-plan.md) for the
phased build and current status.

---

## Path obligations (rules live in `.flowmap.yaml`, not the policy)

Domain lifecycle rules — "our transaction must commit or roll back on every
path", "the audit write must precede the publish" — are evaluated by flowmap,
because only flowmap holds each function's SSA control-flow graph. The rules
ride the service's CODEOWNERS-gated `.flowmap.yaml`:

```yaml
obligations:
  - name: tx-must-close
    acquire: "example.com/svc/internal/store#BeginTx"
    release:
      - "example.com/svc/internal/store#Commit"
      - "example.com/svc/internal/store#Rollback"
  - name: audit-before-publish
    require: "example.com/svc/internal/audit#Write"
    before: "example.com/svc/internal/eventbus#Publish"
```

`flowmap graph` emits per-site verdicts into graph.json's `obligations`
section; groundwork judges them like any other finding:

- `VIOLATED` → gate-failing violation, with the leaking exit as witness.
- `SATISFIED` → silent: the universal proof ("no modeled path leaks") a test
  suite cannot produce. A later SATISFIED→VIOLATED flip surfaces in
  `review`/`verify` as a *new* violation — the drift ratchet at branch
  granularity.
- `CANT-PROVE` → caution: the shape claim would be unsound (resource ownership
  leaves the function, or `recover` is present), disclosed rather than passed.
- `UNMATCHED` → caution: the rule's anchor matches no call site — an inert
  guardrail you must not mistake for protection.

The check is value-blind: it proves the *shape* of the lifecycle, not that the
right value was released. A release performed inside an unlisted helper
reports VIOLATED; the fix is naming the helper as a release ref.

---

## Incident triage (`triage`)

The incident front door: resolve a symptom to suspect functions and read the
blast radius off the graph — throwaway interrogation, no test authoring.

```console
$ groundwork triage --route "POST /api/v1/loans/{id}" graph.json  # failing endpoint
$ groundwork triage --table users graph.json          # corrupted table
$ groundwork triage --frame 'pkg.(*T).Method' graph.json   # panic frame
$ groundwork triage --fail --peer credit-bureau graph.json # what-if: peer down
$ groundwork triage --fail --event loan.approved graph.json
```

Routes are matched segment-wise against the graph's `entrypoints` join, never
exactly-or-nothing: path params on either side are wildcards, a method-less
registration (stdlib `HandleFunc`) matches any queried method, and a mount
prefix on the alert's URL is tolerated (the registration site only ever saw
the leaf pattern). Routers outside root discovery's coverage are absent from
the join — a loud no-match, never a guess.

The card lists the suspects, the implicated entrypoints (their cover), the
upstream callers, the reachable boundary effects, and every blind spot on a
traversed path — the card says where its own claims stop being sound. An
ambiguous symptom returns all candidates (never a guess); an effect the graph
could not name statically (`<dynamic>`) is offered as a flagged *possible*
match. The card is the map (what the suspects could touch), not the route
taken: with an OTel trace of the failing request, `flowmap behavior ingest`
locates the actual divergence inside the suspect set. Triage interrogates the
graph of the *deployed* commit — a stale map mis-triages (stamp graphs in CI
and verify with `--expect`, below). Fault cards also state their epistemic
scope where over-reading happens: causes outside the code (config, infra,
data, deploys) are not on the map, and committed-effect facts cover
same-function orderings only — their absence is never an all-clear.

---

## Suppression and liveness audit (`exceptions`)

Allow-lists accumulate. `groundwork exceptions <policy> <graph>` lists every
active suppression (layering, `must_pass_through`, `blind_spot_ratchet`,
`effect_ratchet`) with its reason, and flags **DEAD** entries — ones that no
longer suppress anything in the current graph. Delete them: a stale excuse can
silently cover a future violation. Read-only, exit 0; the measurable target is
a dead count of zero.

The audit also lists **rule liveness**: every pattern of every `must_not_reach`,
`must_pass_through`, and `no_concurrent_reach` rule, with its binding state
against the graph. This surface is absolute (no base/branch diff), which is
what keeps a *born-inert* rule visible — a rule added already-inert cautions
identically on base and branch, so the review diff suppresses it forever; only
this listing catches it. `from`/`through` patterns that bind nothing are
**DEAD** (fix or delete); `to` patterns that match nothing are **INFO**, since
that may be success (the forbidden thing is absent) or rot (a renamed target) —
the reviewer judges. `--json` emits `{"exceptions": [...], "rule_liveness":
[...]}`.

---

## Bootstrapping a policy (`init`)

The cold-start answer: `groundwork init graph.json --out policy.json --guide
POLICY-GUIDE.md` derives a baseline policy from the service's measured facts —
layers from the package call DAG, a waypoint that already guards every
entrypoint-to-DB-write path, read-only invariants for routes that write
nothing today, the write budget at the current maximum, the existing
blind spots allow-listed observe-first, and the current external write
targets allow-listed observe-first (the effect ratchet). **Everything is a ratchet of current
truth, self-verified clean against the graph it came from**; where the
inference is already violated by current code, the rule is relaxed with a
`baseline at init` allow entry and the guide reports it as a latent finding —
which is signal, not noise. The guide is written for the refining agent: each
section carries its evidence, its "tighten by" steps (`require_proof`,
`gate: true`), and the questions only the team can answer (obligations need
intent, not inference). A CODEOWNER reviews and commits — init proposes from
facts; it never decides.

---

## Pre-edit grounding (`ground`) and the MCP surface (`mcp`)

Deterministic prevention is cheaper than deterministic rejection. The ground
card surfaces, *before* an edit, the same rules that will gate the merge after
it:

```console
$ groundwork ground graph.json '(*example.com/svc/internal/store.Store).Tx' --policy policy.json
```

The card carries the function's identity (signature, tier, policy layer), its
one-hop neighborhood and entrypoint cover, the boundary effects it can reach,
the **binding rules** — layering membership, `must_not_reach` sources,
`must_pass_through` waypoints, `no_concurrent_reach` targets, plus the
graph-borne obligation verdicts and partial-effect facts that bind with no
policy at all — and every blind spot touching those claims. Binding rules are
derived with the exact matchers the checks use, so the card never promises a
guardrail that does not bind.

`groundwork mcp <graph.json> [--policy …] [--corpus <dir>] [--capture <grade>] [--expect …] [--log calls.jsonl]`
serves ten tools over
MCP stdio (newline-delimited JSON-RPC, protocol 2024-11-05, no third-party
dependencies): `ground`, `reach`, `triage` (with the `fail` what-if framing,
including effects possibly committed before the fault), `exceptions`,
`entrypoints` (what the route/event symptoms can address), `fleet-events`,
`chains` (the cross-service effect-chain cards — fleet-wide, like
`fleet-events`), `fitness`, `impeach`, and
`reload`.

The `impeach` tool is **audit-only and never a gate** (it is always listed, but
returns an error unless the server was started with both `--corpus` and `--policy`).
It joins the loaded graph
against a committed `*.golden.json` behavioral corpus and discloses *impeachment
candidates* — effects observed in the corpus where static analysis placed none —
each classified through the downgrade ladder (IMPEACHMENT, or a specific downgrade
like VERSION-SKEW / CAPTURE-UNTRUSTED) with the localized site where static lost
the effect. It runs at `OriginLive` by construction, so it can **never** be read
as a merge verdict: the loaded graph may be a local build, and the deterministic
merge gate is `groundwork verify --corpus` over CI-built base/branch graphs (see
the [structural gate](#the-impeachment--corpus-gate) below), not this lens. The
optional `--capture production|integration` asserts the corpus's fidelity grade
and is reconciled against the grade the corpus self-describes — it requires
`--corpus`, and a contradiction fails closed (the candidate caps below
IMPEACHMENT) rather than laundering a test corpus into a trusted one.

The corpus is a **load-once startup input** (like `--policy`): only the graph is
staleness-tracked and reloadable, because the graph is the one input CI rewrites
under a running server (on redeploy), whereas a committed corpus changes only on a
git action in the checkout — restart to refresh it. The lens discloses this
contract on the card (the source dir and golden count), and the `corpus <digest>`
it prints is the agent's own integrity check: recompute it over the directory to
detect drift since the server loaded it.

A graph file that changes on disk is flagged on every response —
the server never reloads silently; `reload` re-verifies the stamp. `--log`
writes a deterministic transcript (the E4 measurement apparatus): one JSON
line per tool call carrying the raw params, the resolution (the answering
service, `*` for fleet-wide lenses, absent when resolution failed), the
session id, and the isError outcome. Session ids are sequential, minted at
`initialize`, and attribution rides the id rather than line order — so the
shared team server's transcript stays readable when concurrent clients
interleave. No timestamps, so a replayed drill produces identical bytes.
`groundwork transcript calls.jsonl`
is the reader: sessions, per-session query counts, tool/service mix,
cross-service hops, error/correction rates.
**No write tools, ever**: a tool that edited rules would let the
agent author its own guardrails. The
agent's edit loop becomes ground → edit → verify with one rule set at both
ends; the incident loop becomes triage → narrow → `flowmap behavior ingest`.
The server only ever reads the CI-generated graphs it was started with — the
same trust posture as every other groundwork surface.

The `--service` form serves a neighborhood of services in one session:

```console
$ groundwork mcp --service payments=graphs/payments.json \
                 --service ledger=graphs/ledger.json \
                 --policy payments=payments-policy.json \
                 --expect payments="$DEPLOYED_SHA"
```

Each service keeps its own index, policy, stamp, and staleness state; every
tool takes an optional `service` argument, and with a single loaded service
it is never needed (the lone service is the default — the single-graph form
is unchanged). With several loaded, per-service tools require the hop to be
explicit, `entrypoints` with no service lists the whole fleet prefixed by
service, and `fleet-events` joins the graphs' bus surfaces by event name —
who publishes what, who consumes it — the first cross-service lens. The join
vocabulary is the boundary contracts' (published/consumed names match across
services); answers stay per-service and honest. This is **not** a merged
cross-service graph: a side with no loaded match says so rather than
guessing, and dynamically-named publishes are disclosed per service.

`chains` is the second cross-service lens (and a CLI, `groundwork chains`):
for each event it renders a happens-before **chain card** whose links are
labeled **proven** (a per-service graph fact — the producer's in-frame publish
commit ordering and must-precede verdicts, the consumer handler's downstream
effects and obligations) or **assumed** (the broker guarantee, declared in a
`brokers` policy block and printed verbatim, never inferred). It is
observational, never a gate, and honest about the fleet it was handed: a
half-open chain (a publish no loaded service consumes, or vice versa) prints
as open, and an unsigned broker block prints its values flagged UNSIGNED. See
`docs/design/cx5-chains-surface.md`.

`--http <addr> [--token <secret>]` swaps stdio for the **streamable-HTTP
transport** (protocol revision 2025-03-26), turning either form into a
team-shared server — one centrally-managed instance, fed directly by CI
artifacts, answering every agent on the team. This *strengthens* the trust
posture: with stdio the agent's own `.mcp.json` picks the file the server
loads; here the operator picked the inputs and the agent cannot choose them
at all. The server is stateless (one JSON-RPC message per POST, one JSON
response; no SSE streams — no tool ever sends a server-initiated message, so
`GET` is honestly 405). `initialize` returns an `Mcp-Session-Id` that
clients echo on later requests; it is a transcript attribution label only —
the server stores no session state, never requires the header, and a client
that omits it lands in the transcript's anonymous bucket. Auth is one static
bearer token
(`--token` or `$GROUNDWORK_MCP_TOKEN`), compared in constant time and
**required when binding beyond loopback** — an unauthenticated team server
fails at startup, not in production. Browser-borne requests with non-loopback
`Origin` headers are rejected (the spec's DNS-rebinding defense). TLS and
real identity belong to a reverse proxy in front; `GET /healthz` answers
liveness without auth; `SIGINT`/`SIGTERM` drain gracefully.

---

## Integration guide

How to wire the toolset in, end to end. The short version: flowmap runs where
you trust the code checkout (CI), groundwork runs wherever someone needs an
answer, and the only state between them is canonical JSON.

### CI: the structural gate

```yaml
# In the PR pipeline. The graphs MUST be generated here, from checked-out
# source — never accepted from the branch author (see "The trust boundary").
- name: structural gate
  run: |
    git fetch origin "$BASE_REF"
    git worktree add /tmp/base "origin/$BASE_REF"
    flowmap graph /tmp/base/services/payments  > base.json
    flowmap graph       services/payments      > branch.json
    groundwork verify policy.json base.json branch.json   # exits non-zero on BLOCK
    groundwork review policy.json base.json branch.json --json > review-artifact.json
```

`groundwork fitness --sarif` emits SARIF 2.1.0, so obligation violations land
as inline annotations at their witness lines in the PR review UI; the
composite action `.github/actions/setup-groundwork` installs both binaries.
Post `groundwork review`'s text form as the PR comment; archive the `--json`
artifact so any later verifier can run
`groundwork verify-artifact <artifact> <policy> <base> <branch>` and prove it
authentic. Keep `policy.json`, `.flowmap.yaml`, and the gated artifacts under
CODEOWNERS — the rules are reviewed exactly like code, and an agent under
review can never author its own guardrails.

Recommended cadence for adopting checks: start every new rule **observe-only**
(`blind_spot_ratchet.gate: false`, cautions instead of `require_proof`), watch
a week of PRs, then tighten. A gate the team trusts is worth ten gates they
route around.

### Exit codes and outputs

| Command | Exit non-zero when | Machine output |
|---|---|---|
| `fitness` | any Violation finding | text findings (cautions never fail it) |
| `verify` | new violation, scope escape, breaking contract, gated blind spot | `--json` GateResult with digest |
| `review` | verdict is BLOCK | `--json` canonical artifact with digest |
| `diff` | breaking contract change | text |
| `verify-artifact` | artifact tampered or stale | text status |
| `reach`/`triage`/`ground`/`exceptions` | only on bad input | `--json` canonical cards |

A failed verdict exits **1**; an operational failure (bad flags, unreadable
inputs) exits **2** — so CI can tell "the change failed the gate" from "the
gate failed to run". Both are non-zero: a plain pass/fail gate needs no change.

Everything `--json` is canonical (sorted keys, stable bytes) and safe to diff,
cache, or hash.

### Incident runbook hook

Archive `graph.json` per deployed commit (it is small, canonical, and
digest-bearing — the same CI job that gates can upload it). The first three
commands of an incident:

```console
$ groundwork triage --frame "$(head -1 panic.txt)"  graph-$DEPLOYED_SHA.json
$ groundwork triage --fail --peer credit-bureau     graph-$DEPLOYED_SHA.json
$ flowmap behavior ingest --flows-dir flows/ incident-trace.otlp.json
```

Triage interrogates the *deployed* commit's graph — a stale map mis-triages,
which is why the per-deploy archive matters. To make that check mechanical,
stamp the graph in CI and verify at use (opt-in at both ends — no warning
noise on routine local runs):

```console
$ flowmap graph --stamp "$GITHUB_SHA" svc/ > graph-$GITHUB_SHA.json   # CI
$ groundwork triage --expect "$DEPLOYED_SHA" --peer credit-bureau graph.json
$ groundwork mcp graph.json --policy policy.json --expect "$DEPLOYED_SHA"
```

The stamp is caller-supplied, never derived — deriving it would make the graph
a function of more than the code and break byte-identical regeneration.
A mismatch (or a missing stamp under `--expect`) fails loudly: "this is not
the graph for the code you think it is."

### Agents: the MCP server

Serve the lenses to a coding agent (Claude Code shown; any MCP client works):

```json
// .mcp.json in the repo the agent works on
{
  "mcpServers": {
    "groundwork": {
      "command": "groundwork",
      "args": ["mcp", "ci-artifacts/graph.json", "--policy", "policy.json"]
    }
  }
}
```

The agent gets four tools: `ground` (call before editing — what binds this
function), `reach` (blast radius), `triage` (incident card, with `fail` for
the what-if framing and partial-effect answers), and `exceptions` (allow-list
audit). The intended loop is **ground → edit → verify**: the same rule set
that will gate the merge, surfaced before the edit is made. Tool failures
come back as readable tool results the agent can correct from, never protocol
errors. For how an agent should *orchestrate* these tools — the loop, reading
the three-valued verdict, one-symptom triage, the staleness flag — see the
harness-neutral [`agent-workflow.md`](agent-workflow.md) (Claude Code users
also get it as the bundled `groundwork-workflow` skill).

When the agent works across a service boundary (a publisher in one repo, the
consumer in another), serve both maps from one server with `--service` and
the agent orients with `entrypoints` (fleet-wide) and `fleet-events` before
making the explicit per-service hop:

```json
{
  "mcpServers": {
    "groundwork": {
      "command": "groundwork",
      "args": ["mcp",
               "--service", "payments=ci-artifacts/payments.json",
               "--service", "ledger=ci-artifacts/ledger.json",
               "--policy", "payments=payments-policy.json"]
    }
  }
}
```

### Team-shared serving (`--http`)

One operator runs the server next to the CI artifact store; every agent on
the team points at it and none of them chooses what it loads:

```console
# Operator (systemd unit, container, whatever you run daemons with).
# CI's deploy job overwrites the graph files in place; the server flags
# staleness on every answer until someone calls the reload tool.
$ GROUNDWORK_MCP_TOKEN=$(cat /etc/groundwork/token) groundwork mcp \
    --service payments=/srv/graphs/payments.json \
    --service ledger=/srv/graphs/ledger.json \
    --policy payments=/srv/policies/payments.json \
    --expect payments="$DEPLOYED_SHA" \
    --http 127.0.0.1:8137          # reverse proxy terminates TLS in front
```

```json
// Each agent's .mcp.json: a URL, not a command — no file to pick.
{
  "mcpServers": {
    "groundwork": {
      "type": "http",
      "url": "https://groundwork.internal/mcp",
      "headers": {"Authorization": "Bearer <token>"}
    }
  }
}
```

Operationally honest defaults: the token is required the moment the bind
address leaves loopback (startup error, not a production surprise), the
reload tool re-verifies the stamp it was started with unless the call
supplies a new `expect`, and `--log` keeps working — now as the *team's*
usage transcript.

### Consuming graph.json directly

The graph is a stable, versioned interface — you can build your own lenses on
it. Top-level sections:

| Section | What it is | Notes |
|---|---|---|
| `nodes[]` | `{fqn, sig, tier, fallible}` per first-party function | sorted by fqn |
| `edges[]` | `{from, to, tier, boundary?, concurrent?, via?}`; `to` is an FQN or a `boundary:` label | `<dynamic>` in a label = unresolvable target, disclosed; `via` names the reclaimer (`flowmap --reclaim`) that recovered the edge at a dispatch seam — a verdict over it self-discloses as reclaim-informed |
| `blind_spots[]` | `{kind, site, detail}` — where the graph's knowledge stops | the soundness frontier |
| `obligations[]` | `{rule, kind, fn, site, status, detail}` per anchored site | statuses are an open vocabulary: **fail closed on ones you don't recognize** |
| `effect_order[]` | `{fn, effect, effect_site, callee, callee_site, always}` | "effect can/always precedes this fallible call" |
| `entrypoints[]` | `{kind: http\|consumer, name, fn}` — the route/topic → handler join | names are registration-site literals: match segment-wise, never exactly-or-nothing |
| `stamp` | optional caller-supplied identity (the CI commit SHA) | verify with `triage`/`mcp` `--expect`; absent on local/golden builds by design |

Decode strictly (groundwork uses `DisallowUnknownFields`): a schema change you
have not been taught about should fail loudly, not drop fields silently.
Producer and judge deploy in lockstep — that is a feature.

---

## Glossary

The vocabulary of the toolset, grouped so related terms teach each other. The
[concepts-in-five-minutes](#the-concepts-in-five-minutes) section is the mental
model; this is the comprehensive reference. Each entry says what the term *is*
and, where it matters, *why it exists* — because in this toolset a term usually
encodes a deliberate honesty or determinism decision, not just a name.

### The substrate — what the tools reason over

- **flowmap** — the *producer*. Compiles a service (`go/packages → go/ssa →
  go/callgraph`) and emits facts as canonical JSON: the call graph, the boundary
  contract, and the CFG-derived disclosure sections. Runs the analysis; renders
  no verdict.
- **groundwork** — the *judge*. Only ever *reads* flowmap's JSON and renders
  verdicts against a human-authored policy. Never runs flowmap — keeping them
  separate binaries is what lets CI own graph generation (see **trust boundary**).
- **call graph** — the map of what your code *can* do: every node a function,
  every edge one possible call. Computed from source, with no tests run and no
  instrumentation. The substrate everything else composes from.
- **node** — one first-party function in the graph, carrying `fqn`, `sig`,
  `tier`, and `fallible`.
- **edge** — one possible caller→callee call, optionally flagged `boundary`
  (touches the outside world), `concurrent` (crosses a goroutine spawn), or
  `via` (recovered by a reclaimer).
- **FQN (fully-qualified name)** — a function's canonical identity, e.g.
  `(*example.com/svc/internal/store.Store).Tx`. The stable key every diff,
  matcher, and card resolves on — never a line number or arrival order.
- **signature (`sig`)** — the function's Go type signature, carried for display.
- **fallible** — a node whose Go signature returns an `error`. Lets the tools
  trace which error paths reach an entrypoint; it says nothing about whether the
  error is *handled* well.
- **boundary effect** — a typed edge where code touches the world:
  `boundary:db UPDATE users`, `boundary:bus PUBLISH loan.approved`,
  `boundary:credit-bureau GET /score/{id}`. Turns "this function changed" into
  "these routes can now write that table."
- **op key** — the canonical string for one boundary effect or flow exit —
  `PUBLISH loan.approved`, `DB postgres INSERT ledger`,
  `HTTP GET credit-bureau /score/{id}`. One spelling, defined once (see the
  `opkey` parity guard), so the same effect never reads two ways.
- **boundary contract** — the gated inter-service surface: routes served, events
  published/consumed, external dependencies. A pure function of the code, so CI
  regenerates and diffs it (the **currency gate**).
- **canonical IR / canonicalization** — the byte-deterministic normal form every
  artifact is written through (sorted keys, no wall-clock, no map-iteration
  order). The property that makes a digest meaningful and a gate reproducible.

### Reachability and structure

- **entrypoint** — a function nobody calls, where requests enter: an HTTP
  handler, a bus consumer, `main`. The roots of every backward walk.
- **reachability** — walking edges. *Backward* from a function gives the
  entrypoints it is live behind (its **blast radius**); *forward* gives
  everything it can touch. Most surfaces are compositions of these two walks.
- **blast radius** — the transitive set of callers of a function: who breaks if
  it changes. Read it as an upper bound where it crosses a blind spot.
- **route / topic (event)** — an HTTP route (`POST /loans/{id}`) or a bus
  topic/event name; the human-facing names an alert speaks in.
- **entrypoints join** — the resolved `route/topic → handler` mapping, so a
  symptom phrased as a route lands on a function. Names are registration-site
  literals, matched segment-wise (mount prefixes and path IDs tolerated), never
  exactly-or-nothing.
- **tier / salience** — every node and edge carries a tier 1–4 (1 = most
  consequential: publishes, writes, inbound routes; 4 = noise). Keeps cards and
  snapshots focused on what matters.
- **layer / layering** — named package groups (`handler → app → store`) and the
  rule that a call may not skip one. Your architecture's spine, declared in the
  policy and enforced as a gate.

### Precision and the soundness frontier

- **over-approximation** — the graph may include edges that can't actually fire,
  never omit ones that can. This asymmetry is deliberate: it keeps a *proof of
  absence* sound (if the graph shows no path, none exists, outside blind spots).
- **substrate / algorithm (`--algo`)** — which call-graph algorithm built the
  graph: **RTA** (Rapid Type Analysis, the default — precise enough, cheap),
  **VTA** (Variable Type Analysis, RTA-seeded — refines interface-dense dispatch,
  a *blessed* proof substrate), **CHA** (Class Hierarchy Analysis, rootless —
  fallback for a library with no entry points). All three are sound
  over-approximations modulo reflection/unsafe; the graph self-certifies which on
  a `substrate:` line.
- **HighFanOut** — a dynamic-dispatch site with more callees than the threshold
  (default 8): the analyzer pairs every caller with every callee, inflating
  reach. A precision limit, not blindness — disclosed, and tightened by VTA.
- **blind spot** — a place static analysis genuinely cannot see: reflection,
  `unsafe`, dynamic dispatch, a non-constant value. Recorded in `blind_spots[]`,
  and every verdict crossing one says so. The honesty discipline: you always
  know where the map stops being trustworthy.
- **`<dynamic>`** — a boundary label whose target is unresolvable at compile time
  (a topic computed at runtime). The marker is the disclosure — the tool knows a
  publish *happens*, not *to where*.
- **frontier** — the classified set of every place reachability stops, emitted by
  `flowmap frontier` as *measurement, not a gate*. Each marker is binned: **A**
  truly dynamic, **B** reclaimable structure (a severed dispatch seam), **B2**
  consumer-reclaimable (opaque only because the source is non-constant), **C**
  over-approximation (HighFanOut). The point is to distinguish what is *genuinely*
  dynamic from what only *looks* dynamic and can be recovered.
- **attribution loss** — the fraction of entrypoints the frontier confirms are
  severed from their effects. A **lower bound, not a proof**: a 0 is kept honest
  by a third "unconfirmed" state, never read as "nothing severed."
- **reclaimer (`--reclaim`, `via`)** — an opt-in, *sound* pass that recovers
  edges lost at a framework dispatch seam (the oapi strict-server `wrapper→$1`
  hop), each tagged with `via`. Adds only real edges, so a proof of absence over
  a reclaimed graph is at least as strong; the tag lets a *reachable* verdict
  that leaned on a reclaimed edge be audited as such.

### The policy and its checks

- **policy (`policy.json`)** — the single, human-authored, CODEOWNERS-gated
  source of architectural truth. Declarative, validated strictly on load. If the
  agent under review could author it, it would grade its own homework.
- **fitness function** — a deterministic assertion about graph shape that fails
  closed in CI. The unifying primitive: it converts an architectural fact from
  "held in someone's head" to "re-checked on every change." The families:
- **layering** — no call skips a declared layer.
- **must_not_reach** — a negative reachability invariant: "an unauthenticated
  route never reaches the charge API," proven over *all* paths.
- **must_pass_through (waypoint)** — every path from `from` to `to` must cross a
  `through` node: "every entrypoint→DB path goes through the auth check."
- **no_concurrent_reach** — a target must not be reachable across a `concurrent`
  edge: "no DB write from a goroutine."
- **io_budget (write budget)** — a route's write count stays under
  `max_writes_per_route`. Reads as a **lower bound** where the SQL verb is
  unreadable (non-constant SQL) — the check cautions rather than passing falsely.
- **blind_spot_ratchet** — stops dynamic-dispatch blind spots from growing
  change-over-change (`gate: true` to enforce); the anti-erosion lock on the
  graph everything else depends on.
- **effect_ratchet** — stops the external *write surface* from growing
  unreviewed: a new write target (a new table, topic, or peer) base→branch
  needs an `allow` entry (`gate: true` to enforce). (The growth of the
  unclassified-DB / `<dynamic>` *fidelity* fraction is a separate, non-gating
  disclosure — `db_label_drift` — not this ratchet.)
- **require_proof** — on a reach/waypoint rule, upgrades "the static graph found
  no path" to "and the path is fully resolvable." Fails closed through a blind
  spot instead of passing vacuously.
- **allow-list / exception** — a blessed deviation (`{from, to, reason}`) — real
  layered code has legitimate ones (everything calls `Error()`). Review once,
  suppress forever, fail only on *new* violations.
- **exceptions audit / dead entry** — `groundwork exceptions` flags allow-list
  entries that no longer excuse anything, so a blessed exception can't quietly
  rot into a permanent blind spot. Liveness is per-graph.
- **brokers** — declared messaging-channel semantics (delivery `at-least-once` /
  `exactly-once`, ordering, idempotency). A *human warrant* (`signed_by`), since
  the graph can't observe a broker's guarantees.

### Path obligations and partial effects

- **path obligation** — a domain lifecycle rule proven over *every* CFG path by
  flowmap (which holds each function's control-flow graph). Rules live in
  `.flowmap.yaml`, not the policy.
- **acquire / release (must-release)** — "after `BeginTx`, every path commits or
  rolls back." `acquire` opens the obligation; any `release` discharges it.
- **require / before (must-precede)** — "the audit write must precede the
  publish." An ordering obligation over all paths.
- **obligation statuses** — **SATISFIED** (a universal proof no test suite can
  produce — silent), **VIOLATED** (names the leaking exit as witness),
  **CANT-PROVE** (the claim would be unsound — resource escapes the function, or
  `recover` is present — disclosed, not passed), **UNMATCHED** (the anchor binds
  no call site — an inert rule, surfaced as a caution).
- **dominance** — graph-theoretic "happens on every path to here." How a
  partial-effect fact distinguishes *certainly* from *possibly*.
- **effect_order / partial-effect facts** — within one function, whether a
  boundary effect *certainly* (dominating) or *possibly* (branch-arm) precedes a
  fallible call. The incident question "what's already irreversibly done if this
  faulted?" — answered from CFG dominance, not log spelunking. **Same-function
  only**, disclosed on every fault card.

### Verdicts and three-valued honesty

- **three-valued verdict** — nothing answers only pass/fail. The third value —
  *cannot-prove*, *abstain*, *no-signal* — is load-bearing: a tool that cannot
  prove something *says so* instead of passing silently. Green means proven,
  never "nothing noticed."
- **BLOCK / STRUCTURALLY-CLEAR / NO-STRUCTURAL-SIGNAL** — the `review`/`verify`
  verdicts: an invariant or contract broke / shape preserved (*not* a logic
  sign-off) / body-only change the graph abstains on (exits 0, but *not* a clean
  bill of health — send attention to logic).
- **witness** — the concrete evidence a violation prints (the bypassing edge, the
  leaking exit by line). A verdict you can act on without re-deriving it.
- **fail closed / abstain** — when input is incomplete, ambiguous, or
  unverifiable, refuse rather than guess. A surfaced UNKNOWN beats a plausible
  wrong pole.
- **soundness asymmetry** — a *proof* (covered, absent) may only be emitted when
  actually proven; a *negative* follows only from over-approximated reachability
  and holds only outside the blind spots. Everything else is UNKNOWN.
- **scope creep** — a change reaching further than declared (new packages/effects
  beyond a `--scope`). `verify` flags it even when no invariant broke.

### The review artifact and its integrity

- **review artifact** — the computed MR review: verdict, shape, *newly-introduced*
  findings (diffed by canonical identity against base, so old debt is excluded),
  contract movement, I/O effects, reach. Computed *from* the graphs, so the
  author can't embellish it.
- **shape** — the artifact's first-triage label: body-only / localized /
  cross-package / broad. Tells the reviewer how hard to look before opening the
  diff.
- **digest** — `sha256` over the artifact's canonical structural content. Catches
  accidental edits and staleness — but it is **not** the trust anchor (see
  **code-correspondence**).
- **code-correspondence** — the real guarantee: a verifier *recomputes* the
  artifact from the trusted base/branch graphs and compares. This is what catches
  a **re-signed forgery** (body edited *and* digest recomputed) — the digest
  alone cannot.
- **tampered vs stale** — `verify-artifact`'s two failures: **tampered** (body no
  longer hashes to its digest) vs **stale** (digest no longer matches the code's
  recomputation).

### Identity, determinism, and trust

- **determinism / byte-determinism** — every output is a pure function of its
  inputs, byte-identical on any machine. The acceptance bar for every feature:
  anything heuristic or sampled lives *outside* the verdict path, by design.
- **stamp (`--stamp`) / `--expect`** — `flowmap graph --stamp <sha>` records a
  caller-supplied identity in the graph; the gate commands take `--expect <sha>`
  to bind a verdict to the exact code, so a stale graph can't gate the wrong
  code. `GROUNDWORK_REQUIRE_STAMP=1` makes the binding mandatory in CI.
- **trust boundary** — the single load-bearing condition: graphs MUST be
  generated by trusted CI from checked-out source, **never accepted from the
  author/agent under review.** An agent that supplies its own graph forges a pass
  by omitting an edge — groundwork cannot detect that and does not try.
- **human-as-oracle / CODEOWNERS** — a machine verifies everything mechanical; a
  human is the only judge of *intent*. CODEOWNERS routes every gated artifact,
  the policy, and `.flowmap.yaml` to that human, so a rule/contract/golden change
  is unbypassable.

### The lifecycle surfaces

- **reach** — blast radius + entrypoint cover + effects for one function.
- **ground / grounding card** — the pre-edit card: which rules *bind* the
  function you're about to touch, derived with the *same matchers* as the merge
  gate, plus a **`trust: reliable | verify`** flag that fires where the graph is
  unreliable. Prevention before generation.
- **triage / fault card** — the incident front door. Takes the alert's own words
  (**symptom kinds**: `--route`, `--frame`, `--table`, `--event`, `--peer`) and
  returns a bounded suspect set, implicated entrypoints, and (with `--fail`) the
  partial-effect answer. Measured at 10/10 recall, median 8% **hunt space**.
- **hunt space / recall** — triage's two effectiveness measures: the fraction of
  the graph a responder must examine (smaller is better), and whether the true
  culprit is ever outside the suspect set (must be 100%). Both are committed test
  assertions (`drills.md`).
- **verify / review / diff** — the pre-flight gate (new violations, scope creep,
  breaking contract) / the reviewer's computed artifact / the boundary-contract
  diff.
- **init** — proposes a baseline policy from measured facts, allow-listing latent
  violations so the gate fails only on *new* ones. The measure-then-enforce
  on-ramp.
- **chains (CX-5)** — cross-service effect-chain cards (publisher→consumer across
  services). Observational, not a gate.
- **mcp / fleet / fleet-events / transcript** — `groundwork mcp` serves the
  lenses as MCP tools to an agent (stdio or shared HTTP); a **fleet** is several
  services in one server, **fleet-events** the publisher→consumer join across
  them, and `transcript` summarizes a `--log` of a session (sessions, tool mix,
  cross-service hops).

### The behavioral pipeline

- **OpenTelemetry (OTel) / OTLP / span** — the tracing standard flowmap reads.
  Spans (with semantic-convention attributes) are the raw behavioral input; OTLP
  is the wire format for a captured trace export.
- **flow test** — an in-process test that drives a real request through the real
  OTel pipeline and asserts its boundary exits with `Expect`/`ExpectExactlyOnce`.
- **golden / golden snapshot** — the committed canonical record of a flow's
  observed effects (`*.golden.json` + rendered `*.flow.md`). A stale golden fails
  the suite (the **snapshot gate**).
- **determinism self-test** — `Run` re-drives a flow 3× (varying goroutine
  scheduling) and canonicalizes, so a snapshot that varies run-to-run fails
  *before* it's committed.
- **coverage** — the boundary effects no committed flow exercises: the delta
  between what the code *can* do and what the tests *prove* it does.
  Informational — a gap means "write a flow," not "fail the build."
- **post-hoc ingestion** — `flowmap behavior ingest` maps a captured OTLP trace
  (e.g. an incident's) to boundary effects, to locate where a run diverged from
  known-good *inside* the suspect set the graph bounded.
- <a id="the-impeachment--corpus-gate"></a>**the impeachment / corpus gate** —
  `groundwork verify --corpus <dir>` audits the **branch** graph against a
  committed `*.golden.json` behavioral corpus and folds the result into the
  structural verdict (§9). When an effect is **observed** in the corpus but the
  graph proved it absent/unreachable, the static negative is *impeached*: a proof
  the graph relied on (a `must_not_reach` `require_proof`, or a behaviorally-
  witnessed breach) is downgraded SATISFIED→CANT-PROVE or upgraded to VIOLATED,
  and the merge **BLOCKs**. Three fences keep it sound and deterministic:
  **committed-only** (a live corpus is audit-only — it never gates, because it is
  not byte-reproducible); **opt-in** (`impeachment_gate.gate` arms blocking;
  observe-first otherwise — the breach is disclosed but does not block until a
  human ratifies it); and **capture-fidelity** (`--capture production|integration`
  is reconciled against the grade the corpus self-describes, and a contradiction
  fails closed — only a trusted, agreed grade can promote a gating impeachment).
  The same audit is available read-only and gate-free to an agent via the MCP
  `impeach` lens.
- **the four gates** — **currency** (boundary contract is regenerated and
  diffed), **snapshot** (goldens ride `go test`), **structural** (CI regenerates
  base/branch graphs and runs `verify`, optionally `--corpus` for the impeachment
  gate above), and the **impeachment** dimension folded into that structural run.
  Unified only by CODEOWNERS routing and the human-as-oracle — no AI in any of them.

### Behavioral impeachment

- **impeachment** — the join of captured behavior against the static graph that
  asks the one question the analyzer cannot ask itself: *did we observe an effect
  static proved couldn't happen?* It is a **counterexample finder for the
  analyzer's own negatives** — it can find unsoundness only on *exercised* paths
  and never proves static is sound. Not an audit; silence means "no counterexample
  on what you ran," never "the graph is correct."
- **impeachment candidate / witness** — an effect **observed** in the corpus where
  static placed none (proved it unreachable or absent). The witness carries the
  observed flow + entry, the static claim under test, and the localized site.
- **downgrade ladder** — the five-rung classifier that decides whether a candidate
  is a real **IMPEACHMENT** or a benign explanation. A candidate that fails any
  rung downgrades to that rung's disclosure rather than minting a false
  impeachment — fail-closed by construction (an unprovable candidate biases to
  abstention, never to a confident pole).
- **the downgrades** — the benign poles a candidate caps at: **VERSION-SKEW** (code
  identity unestablished — can't prove the trace ran this code), **LABEL-MISMATCH**
  (effect outside the one-source label vocabulary), **CROSS-SERVICE** (effect on a
  foreign service's span), **CAPTURE-UNTRUSTED** (capture not graded
  production/integration). Each is recorded whole, so the abstention is auditable.
- **severance / localization** — *where* static lost the effect: the missed entry
  registration (a **missed root**), the severed emitter, or the unmodeled effect.
  **L0** is the coarse anchor; **L1** resolves it to the exact severed node on the
  observed causal path when `flowmap.fqn` tags are present (sound for
  clean-final-segment first-party code, §12.5). A blind-spot repair self-
  extinguishes exactly this site.
- **capture grade / provenance** — the self-declared capture fidelity
  (**production** | **integration** | **synthetic**). Producer-set, never inferred;
  only production/integration clear the capture-fidelity rung. A `--capture` the
  caller asserts is reconciled against the corpus's own grade and fails CLOSED on a
  contradiction, so a test corpus can never be laundered into a gating impeachment
  (§12.6). Only production/integration may be asserted (`capture.AssertableGrade`).
- **CorpusOrigin (live vs committed)** — the determinism fence: a **committed**
  corpus (`*.golden.json`, byte-reproducible) may gate; a **live** corpus is
  **audit-only by representation** — its verdicts are disclosed but can never move a
  deterministic gate. This is why the MCP `impeach` lens runs `OriginLive`.
- **impeach lens** — `groundwork mcp … --corpus`: the same audit served read-only to
  an agent. It **discloses, never gates** (the loaded graph may be a local build);
  the gate is `verify --corpus` over CI base/branch graphs.
- **observe-first / self-extinguish / ratchet** — the loop's discipline. *Observe-
  first*: an impeachment is disclosed from day one but blocks only once
  `impeachment_gate.gate` is armed and a human ratifies. *Self-extinguish*: the
  proposed repair (declare the blind spot) is verified to actually extinguish the
  witness without minting a new false proof. *Ratchet*: ratifying co-commits the
  allow-list entry and **enacts the seam back into the graph**, so the next build
  abstains there — the graph learns where it was blind.
