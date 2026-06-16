# Working with the groundwork MCP tools (agent workflow)

This is harness-neutral guidance for any coding agent (Claude Code, or any
other MCP client) editing a Go service whose graph is served by
`groundwork mcp`. It covers **how to orchestrate the tools**, not what each
one does — the per-tool descriptions and parameters are authoritative in the
MCP server itself (`cmd/groundwork/mcp.go`) and in
[`usage.md`](usage.md#pre-edit-grounding-ground-and-the-mcp-surface-mcp).
Read this once; let the live tool descriptions carry the parameter detail.

The tools are **read-only by design**. There is no write tool, ever — a tool
that edited rules would let the agent author its own guardrails. Your job is to
*consult* the graph, then edit code with ordinary file tools.

## The edit loop: ground → edit → verify

Before changing a function, call `ground` on its fully-qualified name. The
grounding card surfaces — *before* the edit — the same rules that will gate the
merge after it: layering membership, `must_not_reach` sources,
`must_pass_through` waypoints, `no_concurrent_reach` targets, the graph-borne
obligation verdicts and partial-effect facts, and every blind spot touching
those claims. The binding rules are derived with the exact matchers the gates
use, so the card never promises a guardrail that does not actually bind.

1. **ground** — call on the target FQN *before* editing. Treat its binding
   rules as constraints on your edit, not as background reading.
2. **edit** — make the change with your normal file tools, respecting what
   `ground` reported (don't introduce a path the card says must not exist;
   keep the waypoints it names on the path).
3. **verify** — the merge gate (`groundwork verify`) re-checks the same rule
   set. If your harness can run it, run it; otherwise expect CI to.

Deterministic prevention is cheaper than deterministic rejection: a constraint
read from `ground` is far cheaper than a `verify` BLOCK discovered at merge.

Use `reach` when you need the blast radius of a function (which entrypoints and
upstream callers are implicated, which boundary effects it can reach) before
deciding whether an edit is local or load-bearing.

## Read the three-valued verdict literally

Every claim from these tools is one of three values, and the third is the whole
point:

- **PROVEN / SATISFIED** — the property holds over every path. A universal
  proof, stronger than any test.
- **VIOLATED** — the property is broken, with a named witness (the leaking
  exit, the forbidden edge). Fix it or you will be blocked.
- **CANT-PROVE / NO-STRUCTURAL-SIGNAL** — the graph has nothing to say.

**CANT-PROVE is not a pass.** Do not treat "the graph has nothing to say" as
"looks fine." It means reachability could not answer — often because a blind
spot (dynamic dispatch, a framework seam) sits on the path. Surface it; don't
silently proceed as if it were green.

## Incident triage: triage → narrow → ingest

When investigating an incident rather than editing:

1. **triage** — hand it exactly **one** symptom: `frame` (a stack frame),
   `route`, `table`, `event`, or `peer`. Set `fail=true` for the what-if fault
   framing — it includes the effects that may have *already committed* before
   the fault (e.g. "the publish landed before the charge faulted").
2. **narrow** — use `reach`, `entrypoints`, and (across services) `chains` to
   tighten the suspect set.
3. **ingest** — `flowmap behavior ingest` takes the incident's own OTel trace
   and pinpoints where it diverged from a known-good flow.

`chains` links are labeled **proven** (a per-service graph fact) or **assumed**
(a declared broker guarantee, printed verbatim, never inferred) — believe the
proven links; treat the assumed ones as the human-warranted claims they are.

## Trust the staleness flag; reload deliberately

The server flags staleness on every response and **never reloads silently**. A
stale graph mis-grounds and mis-triages. If a redeploy changed the code, call
`reload` (optionally with `expect` set to the new deployed SHA to re-verify
identity) before trusting further answers. The graph answers for the commit it
was built from — not for whatever is currently on disk.

## Multi-service sessions

With one service loaded, the `service` argument is never needed. With several
loaded (`groundwork mcp --service name=graph.json …`), orient first with
`entrypoints` (fleet-wide) and `fleet-events` before making the explicit
per-service hop; per-service tools require `service` to be named. This is not a
merged cross-service graph — a side with no loaded match says so rather than
guessing.

## In one line

Consult before you edit, read CANT-PROVE as a disclosure rather than a pass,
give triage exactly one symptom, and never trust a graph the server has flagged
stale.
