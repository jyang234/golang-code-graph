# MCP expansion: tiers 2–3 — plan-of-record

**Status:** Tiers 1 and 2 are built. Tier 1: entrypoints, fitness, reload +
staleness flagging, --log transcript. Tier 2: `--service` fleet serving with
the optional `service` arg on every tool, the fleet-wide prefixed
`entrypoints` listing, and the `fleet-events` lens — implemented in the
pressure-tested shape recorded below (the lone deviation: the lone service
is keyed by its graph path in the single-graph form, so one code shape
serves both). Tier 3 is now also built — see its section for what was paid
and what was deliberately not.

## Tier 2 — multi-service serving (BUILT)

`groundwork mcp --service payments=graphs/payments.json --service
ledger=graphs/ledger.json [--policy payments=...]` — a map of named services,
each with its own index/policy/stamp/mtime state (the existing `mcpServer`
becomes the per-service value). Every tool gains an optional `service` arg;
single-service invocations keep today's behavior with zero changes (the lone
service is the default — no breaking change). `entrypoints` with no service
arg lists across the fleet, prefixed.

**What this is and is not:** it lets one session HOLD the neighborhood's maps
so the agent can walk a publisher in service A to the consumer in service B —
the hop is explicit and the reasoning stays per-service and honest. It is NOT
the fleet model (merged cross-service graph, system-level policy); that is
the larger missing piece named in the gap analysis, and serving N maps is its
transport, not its substance. Scoping note: the boundary contracts are the
join vocabulary (published/consumed events match across services); a
`fleet-events` discovery tool (which service publishes/consumes each event,
from the contracts) is the cheap first cross-service lens and needs no merged
graph. As built, fleet-events reads the same vocabulary from the loaded
graphs themselves (PUBLISH/CONSUME boundary edges + consumer entrypoints) —
the server already holds them, so no extra contract files are taken — and
discloses dynamically-named bus effects per service.

## Tier 3 — streamable-HTTP transport for a team-shared server (BUILT)

A centrally-managed server fed directly by CI artifacts. The pressure-test
finding stands: this STRENGTHENS the trust posture — today the agent's
.mcp.json picks the file the server loads (claim-chain trust); a central
server means the agent cannot choose its inputs at all, and the per-deploy
graph archive becomes self-serving. Costs that deferred it: auth, lifecycle,
the toolset's first deployment-shaped component. The original gate — **build
only alongside a real adopter who needs shared serving** — was satisfied by
the owner asking for it directly.

As built (`--http <addr> [--token <secret>]` on either mcp form): the named
costs were paid minimally and everything else was refused. Auth is one
static bearer token, constant-time compared, REQUIRED when binding beyond
loopback (startup error, fail-closed); TLS and identity are a reverse
proxy's job, not this binary's. Lifecycle is graceful SIGINT/SIGTERM drain
plus an unauthenticated /healthz. The server is stateless on purpose — one
JSON-RPC message per POST, one JSON response, protocol revision 2025-03-26,
no sessions, no SSE streams (no tool ever sends a server-initiated message,
so GET is honestly 405), non-loopback Origins rejected per the spec's
DNS-rebinding defense. Both transports share one dispatch; tool calls are
serialized under a mutex because reload mutates per-service state.

## Standing refusals (decided, recorded in the server's doc comment)

No write tools, ever (the agent must never author its own guardrails); no
graph generation in the server (producer stays in CLI/CI); no free-form graph
query language (cards are curated lenses with disclosure built in); MCP
prompts deferred (methodology belongs to the team, not the instrument).

## Order of evidence (now: evidence after building)

Both tiers were built ahead of the E4 transcript evidence, on the owner's
direct call — the ROI gate was overridden, not satisfied, and that should be
recorded plainly. The measurement plan inverts rather than disappears: the
E4 drill's --log transcripts (which the HTTP transport keeps, now as a
team-wide record) will show whether agents actually make cross-service hops
and whether shared serving earns its keep. If the transcripts come back
empty, the honest move is documented retirement, the same standard every
other surface in this repo is held to.

The instrument for that decision is built: each transcript line now carries
its resolution (answering service, fleet-wide, or failed) and isError
outcome, an init marker bounds sessions, and `groundwork transcript
calls.jsonl [--json]` computes the keep/retire evidence — per-session query
counts, tool/service mix, cross-service hops, correction rates —
deterministically (no timestamps; a replayed drill produces identical
bytes). What it deliberately does not compute: value. Whether conclusions
cite card facts is E4's human-judged half, and the card says so.
