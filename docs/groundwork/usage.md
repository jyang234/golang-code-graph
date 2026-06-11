# groundwork — usage

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
  `concurrent` flags), and a blind-spot manifest. `flowmap boundary <service>`
  emits the gated inter-service **boundary contract** (routes, published/consumed
  events, external dependencies).
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

---

## The policy

The policy is the single, human-authored source of architectural truth. It is a
gated artifact — if the agent under review could author it, it would grade its
own homework — so it is declarative and validated strictly on load.

```json
{
  "service": "layeredsvc",
  "version": 1,
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

It declares six invariant families:

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
groundwork triage (--frame|--table|--event|--peer) <v> [--fail] <graph>   incident triage card
groundwork exceptions <policy> <graph>                  audit allow-lists; flag dead entries
groundwork fitness <policy> <graph>                     evaluate invariants against one graph
groundwork review <policy> <base> <branch> [--json]     computed MR review artifact
groundwork verify <policy> <base> <branch> [--scope …]  fail-closed pre-flight gate
groundwork diff <base-contract> <branch-contract>       inter-service contract diff
groundwork verify-artifact <artifact> <policy> <base> <branch>   prove an artifact authentic
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
fitness OK — 3 invariant(s) hold, 0 caution(s)
```

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
digest ee405bceee1a1949… · recompute to verify (deterministic; not author-editable)

Shape: cross-package
Touches: handler(+1)

⛔ Introduces 1 invariant violation(s)
- layering — handler → store skips 1 layer(s)
  - (*…/handler.Server).GetUserFast → (*…/store.Store).SelectUser

🔌 External contract changed (additive)
- + entrypoint handler.Server.GetUserFast
```

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

### `verify` — the fail-closed pre-flight gate

The gate form of `review`. It blocks a merge on a new violation, on a **breaking
contract change**, or on **scope creep** — a touched package outside the declared
`--scope`:

```console
$ groundwork verify policy.json base.json branch.json \
      --scope example.com/layeredsvc/internal/handler
# Pre-flight gate — BLOCK
digest b0e5045282859c4b… · recompute to verify (deterministic; not author-editable)

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
$ groundwork triage --table users graph.json          # corrupted table
$ groundwork triage --frame 'pkg.(*T).Method' graph.json   # panic frame
$ groundwork triage --fail --peer credit-bureau graph.json # what-if: peer down
$ groundwork triage --fail --event loan.approved graph.json
```

The card lists the suspects, the implicated entrypoints (their cover), the
upstream callers, the reachable boundary effects, and every blind spot on a
traversed path — the card says where its own claims stop being sound. An
ambiguous symptom returns all candidates (never a guess); an effect the graph
could not name statically (`<dynamic>`) is offered as a flagged *possible*
match. The card is the map (what the suspects could touch), not the route
taken: with an OTel trace of the failing request, `flowmap behavior ingest`
locates the actual divergence inside the suspect set. Triage interrogates the
graph of the *deployed* commit — a stale map mis-triages.

---

## Suppression audit (`exceptions`)

Allow-lists accumulate. `groundwork exceptions <policy> <graph>` lists every
active suppression (layering, `must_pass_through`, `blind_spot_ratchet`) with
its reason, and flags **DEAD** entries — ones that no longer suppress anything
in the current graph. Delete them: a stale excuse can silently cover a future
violation. Read-only, exit 0; the measurable target is a dead count of zero.
