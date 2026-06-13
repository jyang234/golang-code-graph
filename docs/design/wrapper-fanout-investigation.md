# Investigation: the HTTP-wrapper HighFanOut that gates the CX lifts (D-CX10)

**Status:** investigated, measured, recommendation below. No engine change shipped
yet — the deeper fix is a framework-wide decision for the owner (§4).

The second field run (correctness-expansion-plan §2c) found the interprocedural
lifts (CX-2 entry domination, CX-3 effect derivation) abstaining at one
chokepoint: `doPublish`/`publishWithFanout`, a chi/oapi wrapper that is a
HighFanOut blind spot. The lifts are sound — they refuse to over-claim through a
disclosed frontier — so the question (the field's ask #2) is whether the
frontier can be *shrunk*, and how. This note answers it with a measurement, not
a guess.

## 1. The mechanism (verified in code)

- **HighFanOut** (`internal/static/blindspots/blindspots.go:141-158`) flags any
  single SSA call site whose distinct in-graph callee count exceeds the
  threshold (default 8), attributed to the containing function.
- **The wrapper site.** A shared middleware's `next.ServeHTTP(w, r)` is one
  invoke instruction on an `http.Handler` interface value. The same middleware
  wraps every route, so `next` is bound, across the program, to *every* handler.
- **Root discovery deliberately skips middleware**
  (`internal/static/roots/roots.go`, `variadicLastFunc`): a route is rooted at
  its final handler only; the `Use(...)` stack and chain order are never
  recovered. So the route→handler endpoints exist (`Entrypoints`), but the
  middleware *chain* — what a router-aware refinement would need — does not.
- **VTA exists, unexposed.** `internal/static/callgraph/callgraph.go` has
  `AlgoVTA` (RTA-seeded variable-type analysis); `flowmap graph` always uses the
  `Options{}` zero value, RTA.

## 2. The measurement (`callgraph/wrapperfanout_test.go`, committed, deterministic)

A faithful reproduction — one shared `mw{next Handler}` whose `Serve` calls
`next.Serve()`, wrapping ten handlers `H0..H9`, the oapi/chi shape — measured at
the `next.Serve()` site:

| algorithm | distinct callees at the shared site |
|---|---|
| **RTA** (default) | **22** |
| **VTA** (refine) | **10** |

Two facts, both load-bearing:

1. **VTA materially helps.** It removes the 12 spurious candidates RTA carries —
   the `mw` type itself (which implements `Handler` and flows to the interface)
   and the SSA method-value/wrapper thunks — landing on exactly the ten real
   handlers `next` can hold. Soundly: VTA tracks which concrete types flow to
   `next` and finds only `H0..H9`.
2. **Neither prunes below ten.** At a genuinely-shared middleware, `next`
   *dynamically does* range over all ten handlers. The width is real program
   behavior, not over-approximation. With threshold 8, the blind spot **still
   fires under VTA** (10 > 8).

## 3. What this rules in and out

- **Router-aware chain recovery is NOT the lever.** It needs infrastructure that
  doesn't exist (middleware-stack extraction, per-router and brittle), and the
  measurement shows it wouldn't change the truth anyway: a shared middleware's
  `next` reaches every handler regardless of how perfectly the chain is modeled.
  Recovery would only help the *distinct-wrapper-per-route* shape, which the
  shared-middleware oapi/chi pattern is not. Drop it.
- **VTA is a real, cheap win — but partial.** It roughly halves the fan-out and
  removes the junk candidates, so borderline sites (real callee count ≤
  threshold, inflated by RTA over the line) would clear and un-blind their cones
  for the lifts. It will not rescue a middleware that genuinely fans to more than
  `threshold` handlers.
- **The deeper conflation.** HighFanOut bundles two different things under one
  "the call graph may be over-approximated here" disclosure:
  - **truly blind** — reflect, `<dynamic>` boundary, unresolved invoke: edges are
    *hidden*; reasoning through it is unsound (the lifts must abstain);
  - **wide but fully resolved** — a shared middleware whose ten callees are all
    *enumerated*: nothing is hidden; the callee set is complete (a sound superset
    under RTA, the exact set under VTA). Reasoning through it is *not* unsound —
    the summary engine already requires a property to hold for *all* candidates,
    which is exactly right here. The lift abstains only because the blind-spot
    label does not distinguish the two cases.

## 4. The lift abstention reproduced — and both levers refuted

Before recommending an unlock I reproduced the field's abstention in the
summary engine and ran each proposed lever against it
(`obligations/wrapperabstention_test.go`, committed). The hypothesis in an
earlier draft of this note — that a `resolved-wide` reclassification, or VTA,
would let the lifts fire at `doPublish` — **did not survive the measurement.**

Two faithful shapes, both abstaining UNKNOWN under **both RTA and VTA**:

| shape | why it abstains | lever that would fix it |
|---|---|---|
| shared middleware (`mw.Serve` → `next.Serve`, `next` resolving back to `mw`) | self-referential SCC → recursion guard (D-CX1) | none measured: VTA does not break the self-loop in the engine's resolution |
| handler stored as a func value in a router (`r.handle(pubHandler)`) — **the real oapi/chi shape** | the handler's address is taken → it can be invoked from framework code the unit cannot see | none: address-taking is a genuine soundness boundary |

Neither abstention is the lift being over-conservative about a fully-resolved
site (the case the `resolved-wide` split targets). The summary engine
*already* reasons soundly over a wide-but-resolved dispatch — `addressTaken`
and `dynInvoke` are both false in the SCC case, so the split would not change
its verdict. Both abstentions are **correct**: across a framework dispatch
boundary the analysis genuinely cannot prove what precedes the handler.

## 5. Recommendation (corrected)

1. **The lift abstention at the wrapper is the honest end state, not a gap to
   engineer away.** The lifts deliver where dispatch is statically resolved —
   the field confirmed it (the orchestrator took every derived row, clean
   subgraphs SATISFY). Across the router/wrapper they abstain, and that is the
   framework being truthful. The lever is **rule anchoring**: keep a
   must-precede rule's require and before on the same resolved side of the
   dispatch boundary (the handler body and below), not spanning
   `doPublish`→`publishWithFanout` through the wrapper. This is authoring
   guidance + a documented limit, **no engine change** — and it is what to tell
   the field.
2. **Expose `--algo` on `flowmap graph` — keep, but demoted.** VTA's 22→10 is a
   real precision win (it removes spurious candidates, helps blind-spot *noise*
   and any check that counts callees) and is cheap. But it is **not** a lift
   unlock — measured, §4 — so it ships as a modest precision option, not as the
   answer to ask #2. Default stays RTA.
3. **Drop the `resolved-wide`/`blind` split from the critical path.** It was
   premised on the lift abstaining at a fully-resolved site; the reproduction
   shows it does not. The split may still have independent value for
   groundwork's *frontier checks* (which do treat HighFanOut as blind), but
   that is a separate, lower-priority question — not the field's lift problem,
   and not worth its framework-wide blast radius on this evidence.
4. **Router-aware chain recovery: still out** (§3), now doubly so — even with a
   perfect chain, the address-taken-handler boundary (the dominant real shape)
   remains.

**The framing correction stands and is now measured:** "credit through the
frontier" is wrong because the frontier is real (address-taking,
self-referential dispatch), not because the site is merely wide. The honest
product answer is that the lifts are a *resolved-cone* instrument, and the
wrapper boundary is where they correctly fall silent.
