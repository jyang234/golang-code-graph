---
name: groundwork-workflow
description: Orchestrate the groundwork/flowmap MCP tools when editing or debugging a Go service whose call graph is served by `groundwork mcp`. Use when about to edit a Go function (call `ground` first), when triaging an incident from a symptom (route/event/table/peer/stack frame), or when interpreting a PROVEN/VIOLATED/CANT-PROVE verdict. Teaches the ground → edit → verify loop and how to read the three-valued verdicts — it does not duplicate per-tool parameters, which live in the live MCP tool descriptions.
---

# groundwork workflow

`groundwork mcp` serves a Go service's call graph as **read-only** MCP tools
(`ground`, `reach`, `annotate`, `triage`, `entrypoints`, `fleet-events`,
`chains`, `fitness`, `reload`, `exceptions`, and — only with `--corpus` — the
audit-only `impeach`). There is no write tool — you consult the graph, then
edit code with normal file tools.

The full, harness-neutral guidance lives in
[`docs/groundwork/agent-workflow.md`](../../../docs/groundwork/agent-workflow.md).
Read it for the detail; the essentials are below. **Per-tool parameters are
authoritative in the live MCP tool descriptions** — don't rely on a copy here.

## The three things that matter

1. **ground → edit → verify.** Before editing a Go function, call `ground` on
   its fully-qualified name and treat its binding rules (layering,
   `must_not_reach`, `must_pass_through`, `no_concurrent_reach`, obligation
   verdicts, blind spots) as constraints on your edit. The merge gate
   (`groundwork verify`) re-checks the same rules — surfacing them first is
   cheaper than a BLOCK later. Use `reach` for blast radius before deciding an
   edit is local.

2. **CANT-PROVE is not a pass.** Verdicts are three-valued: PROVEN (holds over
   every path), VIOLATED (broken, with a named witness), and
   CANT-PROVE / NO-STRUCTURAL-SIGNAL (the graph has nothing to say — usually a
   blind spot on the path). Surface CANT-PROVE as a disclosure; never report it
   as "looks fine."

3. **One symptom for triage; trust the staleness flag.** Give `triage` exactly
   one of `frame`/`route`/`table`/`event`/`peer`, and `fail=true` for the
   what-if framing (effects that may have committed before the fault). The
   server flags staleness on every response and never reloads silently — if a
   redeploy changed the code, `reload` before trusting more answers.
