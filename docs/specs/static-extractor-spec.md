# Static Extractor — Specification

> **`ACTIVE`** · component specification (source of truth) · _reviewed 2026-06-13_

The static pipeline. From the repo's code it builds a function-level call graph (via `go/ssa` + `go/callgraph`), then derives two distinct outputs from it:

- **The gated artifact — the inter-service boundary contract** (plus its blind-spot manifest): the events the service publishes and consumes, its external-service dependencies, and its exposed entry points. This is stable under internal refactoring, and it is what routes to review.
- **A generated, non-gated artifact — the full call graph + signatures**: the "what *can* happen / design structure" map, for human understanding and (as structured IR) the AI-assist surface. Regenerated on demand; **not** gated, because function-level structure churns under the refactoring AI does constantly.

This is the **static, exhaustive-but-shallow** half of the system — it sees every statically-reachable boundary call, including error and rarely-hit paths no test exercises — and it is **complementary, not symmetric,** to the **behavioral, deep-but-sampled** snapshots (see the scope & guarantees doc). DB operations and internal ordering are owned by the behavioral snapshot (observed at runtime, tiered), not by this pipeline. It feeds three consumers: human review (of the boundary), service documentation (the graph), and the AI-assist surface (the structured IR).

---

## 1. Pipeline

`go/packages` (load + type-check the repo) → `go/ssa` (build SSA, with `InstantiateGenerics` so calls through generic code are captured) → `go/callgraph`. The graph's `Node.In` / `Node.Out` are upstream / downstream directly; `Edge.Site` maps each edge to its call instruction, hence to an AST position for the optional detail sidecar (§8).

---

## 2. Roots — the entry-point problem

RTA/VTA need roots, and the roots that matter aren't only `main`. For an event-driven service the real entry points are the **HTTP handlers and bus consumers**, and frameworks register those via dynamic dispatch (`router.HandleFunc`, `bus.Subscribe`) — so they're often *reachable-but-disconnected* in a naive graph. Treat them as **synthetic roots**, identified through the classification hints (which symbols register handlers/consumers; the func-value argument is the root).

`roots = mains ∪ HTTP handlers ∪ bus consumers ∪ (libraries) exported funcs`

Registration patterns vary. flowmap recognizes stdlib `(*http.ServeMux).HandleFunc`
(method in the route string, `"POST /x"`) and **go-chi's per-method router**
(`r.Post`/`r.Get`, the method implied by the function name) — the latter is how
**oapi-codegen's chi server** registers handlers. Two wrinkles that pattern adds
are handled: chi's `Router` is an *interface*, so registration is an
interface-method *invoke* (no static callee) rather than a direct call; and the
route is a `baseURL + "/path"` concatenation, so the route template is recovered
from the constant segments (eliding the non-constant base URL). The handler is
the generated wrapper method, which reaches the real implementation through the
`ServerInterface` — connected by RTA like any other interface call. The same
concatenation recovery makes oapi-codegen's **std-net/http** server work too
(`m.HandleFunc("GET "+baseURL+"/path", wrapper.X)`).

Other method-named routers (echo, custom) are declarable via `static.routers` in
`.flowmap.yaml` (package + the registration function names; the HTTP method is
the name uppercased). A matched call is only treated as a registration when its
handler argument is **func-typed** — so an incidental name collision (a config
registrar matching, say, a `cache.Get`) is skipped silently rather than
mis-recorded as a gated blind spot, while a genuinely dynamic func handler is
still disclosed. **gin** is covered too: its handler is variadic, arriving as a
slice the caller builds, from which the final handler (after any middleware) is
recovered and rooted. The one shape still out of scope is **gorilla/mux**, where
the HTTP method comes from a chained `.Methods(...)` call on the returned
`*Route` rather than the function name or route — it needs the chained call
followed, not just a config hint.

This **aligns the static graph with the behavioral flows**: the static roots (handlers, consumers, mains) mirror the behavioral triggers (HTTP, event). Organize the graph **per entry point** — the subtree reachable from each handler/consumer — so the static artifact and the per-flow snapshots are about the same units.

---

## 3. Algorithm dial (soundness vs. precision)

- **Default:** RTA from the discovered roots (services); VTA to refine interprocedural type flow.
- **Fallback:** CHA / root-at-all-exports for library packages with no entry points — noisier, disclosed.
- Record which algorithm ran and the resulting caveats. Configurable override.

The tradeoff is real: `static` misses all dynamic dispatch; CHA is sound but over-approximates; RTA is precise but needs roots; pointer analysis is most precise and most expensive. RTA-from-discovered-roots is the sweet spot for services.

---

## 4. Scoping at the repo boundary

- **Nodes:** first-party functions (the repo's modules/packages).
- **Boundary edges:** calls out to stdlib / third-party / DB / bus / downstream are recorded as **typed edges to boundary nodes** — classified (DB write, publish, stdlib call, downstream RPC) — but **not traversed into**. The graph is "first-party call structure + typed boundary edges," mirroring the behavioral model where a flow ends at its boundary I/O.
- **Derived inter-service boundary contract (the gated artifact):** from the boundary edges — events **published** (outbound-async), events **consumed** (the consumer roots), external **service dependencies** (downstream RPC/HTTP targets), and the **exposed entry points** (routes/consumers). This is the surface stable under internal refactoring, and it answers "did this change what the service does to the world," so it is what's committed and routed to review. Emit it as **machine-joinable metadata** — the cross-repo composition seam: matching one repo's published events to another's consumed events reconstructs choreography without cross-repo tracing.
  - **DB operations are *not* in this contract.** The database is the service's own store, not an inter-service surface, and DB calls churn under query refactoring; they are owned by the behavioral snapshot, which observes them at runtime and tiers them. (See the scope & guarantees doc for the two distinct meanings of "contract.")
  - **Exhaustive only over statically-resolvable paths.** A dynamically-constructed target — `publish(fmt.Sprintf("%s.completed", entity))`, or interface dispatch the algorithm can't pin — cannot be named here. That gap is recorded in the boundary blind-spot manifest (§7), not silently omitted.

---

## 5. Feature extraction → tier (linkage to the tier-map)

Per edge (caller F → callee G), compute the normalized features the shared classifier consumes, then call `Classify`:

| Feature | Static source |
|---|---|
| Boundary | F-pkg vs G-pkg (same-package / cross-package / internal); G in an I/O-hint pkg → outbound-sync/async; edge from a handler/consumer root → inbound |
| Effect | classification hints (telemetry/db/bus) + method-level (`db.Exec`→mutate, `db.Query`→read, `bus.Publish`→mutate/async) |
| Origin | G's import path → stdlib / first-party / third-party / same-package |
| Fallible | G's signature returns `error` |
| Concurrent | call site inside a `go` / `defer` (from SSA) |
| Identity | G's fully-qualified name |

Whole-program effect inference is **not** attempted: `mutate`/`read` are known at the *boundary* via hints, and first-party internals fall to `compute` → tier 3. Honest and tractable — the consequential effects live at the boundary, which the hints classify.

---

## 6. Signatures

Per function, from `go/types`: parameters and result types, error-returning, receiver, and generic type parameters, rendered as canonical package-qualified strings. This is the "what each function accepts and returns" half of the relationship map.

---

## 7. Blind-spot manifest (the honesty requirement)

Emitted alongside the graph so a reviewer never operates on false completeness:

- **Unresolved dynamic call sites** — interface/func-value calls not resolved to a concrete callee (`UnresolvedCall`; the boundary-level form is `UnresolvedDispatch`).
- **`ConcurrentDispatch`** — the goroutine sibling of `UnresolvedCall`: an unresolved func-value call whose dispatching instruction is a `go` statement. The concurrency is a verified SSA fact, so the machine states "an asynchronous body is hidden here" rather than the generic "a call is hidden"; same graph-completeness gap, recovered shape.
- **`ExternalBoundaryCall`** — a hand-off from first-party code into a THIRD-PARTY (non-stdlib) package that is not already a classified boundary effect (HTTP/DB/bus/telemetry): the unclassified external-dependency surface. The callee is KNOWN (its package is named) but its body is outside the analyzed module. **Disclosure-only**: it names a leaf the call graph already stops at, so unlike the unresolved kinds it does NOT blind a `must_not_reach` proof and does NOT enter the frontier/severance metric (`blindspots.Kind.IsDisclosureOnlyFrontier`). Stdlib, classified boundaries, and observability instrumentation (OpenTelemetry built-in, plus `static.externalBoundaryExempt` prefixes) are excluded. Each carries a `severity` **signal/noise tier** — `effect-bearing` (the default — a dependency whose handoff can carry a real external effect, and any package not known to be benign) or `trivial` (pure-compute / framework plumbing: UUID generation, a router's helpers, a codegen runtime — built-in, extended by `static.externalBoundaryTrivial`). The tier is **disclosure-only too**: it never gates and never changes the count (a trivial spot is still detected and counted, unlike an *exempt* one, which is suppressed) — it lets a bare blind-spot count be read as signal vs. noise. The target package rides as a structured `package` field (not re-parsed from the Detail prose), so the Mermaid view labels each boundary node with its dependency — an effect-bearing cloud-SDK seam reads apart from a trivial `uuid` seam at a glance.
- **Over-approximation** — interfaces with many implementers where the algorithm added many candidate edges (flag high fan-out).
- **`reflect`** usage — invisible to the call graph.
- **`unsafe` / `go:linkname` / cgo** boundaries — can hide edges.
- **`ImpeachmentSeam`** — a human-RATIFIED seam (see §7.1), distinct from the others in that it is *declared in config*, not auto-detected from code.

This parallels canonicalization's `Complete` flag and the harness's truncation handling: every component discloses where it's uncertain. The kind set is enumerated by `blindspots.Kinds()` and validated by `blindspots.Recognized`; one comparator, `blindspots.SortBlindSpots` (by Kind, Site, Detail), is the single source for every blind-spot ordering, so the manifest's bytes never depend on detection or declaration order.

**The boundary subset of this manifest is part of the gated artifact.** A dynamically-constructed event name or an unresolved dispatch *at the boundary* is a tracked, reviewable fact: if a PR introduces one, the gated artifact changes and routes to a human ("this PR added an outbound effect we can't statically verify"). That turns the one genuine hole — a dynamically-named boundary effect on a path no test exercises, invisible to both pipelines — into a flagged fact instead of a silent miss.

### 7.1 Declared blind spots — human-ratified seams (impeachment enactment)

Beyond the auto-detected categories, the config may **declare** blind spots: `static.declaredBlindSpots`, each a `{site, kind?, reason}`. These are the enactment half of the behavioral-impeachment loop (impeachment plan §8): a site where *behavior* proved the static over-approximation's disclosure incomplete, ratified by a CODEOWNER. `graphio.mergeDeclaredBlindSpots` folds each into the graph's blind spots so the next run abstains at the seam (a `NEVER` weakens to `CANT-PROVE` — the safe direction; a declared seam can only weaken proofs, never hide a violation, since reachability is edge-based). A consumer (groundwork) cannot tell a declared seam from an auto-detected one.

Fail-closed validation, so a seam can never be a silent or malformed disclosure:

- **`site`** is required (nothing to blind without it).
- **`reason`** is required — the impeachment witness; a seam blinded without a stated reason is drift, not a ratified disclosure. (Both enforced at config load.)
- **`kind`** defaults to `ImpeachmentSeam` and, when set, must name a recognized `blindspots.Kind`; an unrecognized kind is a config error, never a silent passthrough onto the gated artifact.

The merge dedups by `(kind, site)` keeping the lexically-smallest `Detail` — an **intrinsic** tie-break (never arrival order), so the merged manifest is byte-identical regardless of declaration order. An `ImpeachmentSeam` is **not** part of the gated boundary subset (`Boundary()` excludes it) and is **excluded from the blind-spot ratchet** (`review.newBlindSpots`) and from the frontier reclaim markers (`frontier.Classify`): a ratified seam is a reviewed disclosure, not undisclosed-dynamism drift, so ratcheting on it would make the enactment self-defeating (the seam would re-block the very change that ratified it).

### 7.2 Annotations — human/AI context on a blind spot

A blind spot states *where* the analysis stops; an **annotation** supplies *what* lies beyond it — the behavior behind a vendored SDK, the work inside a goroutine — that the machine cannot see. Configured as `static.annotations`, each a `{site, kind?, note, by?, claim?}`, and folded into the graph's `annotations` section by `graphio.mergeAnnotations`, keyed by `(site, kind)` to the blind-spot manifest above.

**Disclosure-only, by construction.** An annotation never closes a blind spot, changes a count, or moves a verdict — it decorates the disclosure channel (the Mermaid render, and groundwork's `ground`/`triage` cards), nothing more. The merge runs *after* the manifest is final and writes a separate section; no reachability, gate, or count reads it.

Fail-closed and deterministic:

- **`site`** and **`note`** are required (an annotation with nothing to attach to, or no context, is drift — enforced at config load).
- The `(site, kind)` must match a detected blind spot. `kind` may be omitted when the site has exactly one; a multi-kind site requires it; an annotation matching no blind spot **fails the build** (a stale FQN cannot silently attach to a vanished site). The binding rule is `config.ResolveAnnotationKind`, shared with the read-only MCP `annotate` proposer so a proposal it accepts is one the build accepts.
- Collisions on `(site, kind)` dedup to the lexically-smallest `(note, by, claim)` — a **total** intrinsic tie-break, never config-array position.

**`claim` (optional)** is a falsifiable, machine-checkable form of the note: the canonical effect key the seam is asserted to hide (`PUBLISH email.sent`, `db DELETE ledger`). Against a behavioral corpus, groundwork's `impeach` lens grades it **CONFIRMED** (the corpus observed that exact effect severed at the site), **UNCONFIRMED** (the seam is witnessed but the claimed effect was not observed — asymmetric: a sample's silence is never proof of absence, so never "false"), or **WITNESSED/UNWITNESSED** for an unclaimed annotation. The grading is audit-only: even a CONFIRMED claim is a stronger *disclosure*, never a proof, and never feeds a gate.

---

## 8. Determinism & serialization

- **The gated artifact** is the inter-service boundary contract (§4) plus its blind-spot manifest (§7) — sorted, position-insensitive, canonical JSON. It diffs only on a genuine boundary change, which is what keeps it low-churn enough to route to a human without training rubber-stamping.
- **The generated, non-gated artifacts** — the full call graph, signatures, and the detail sidecar's source positions — are regenerated on demand and **not** gated, so the function-level churn from renames, extractions, and moves never reaches the gate. Publish them as a CI build artifact and link them from the service README so they stay discoverable without polluting diffs.
- Canonical JSON, sorted keys throughout — the same determinism discipline as canonicalization (Go map iteration is randomized; sort everything).

---

## 9. Outputs and queries

- The **boundary contract + blind-spot manifest** — committed and **currency-gated**: regeneration is a pure function of code, so a stale artifact is caught by regenerate-and-`git diff --exit-code`, while CODEOWNERS routes boundary changes to a human. This is the *currency* gate mechanism, **distinct from the behavioral pipeline's *snapshot-assertion* gate** — both run, and in v1 both are author-regenerated with the CI checks as the staleness backstop (see the scope & guarantees doc).
- The **full call graph + signatures** — generated on demand, published as a CI artifact, **not gated** (function-level structure is too volatile to gate).
- The **Mermaid flowchart view** (`flowmap graph --mermaid`) — the human-readable rendering of the same non-gated graph: typed boundary effects as shaped leaf nodes (DB / bus / external), and the blind spots and frontier markers as explicit terminal nodes, so a reviewer sees *where the analysis stops* rather than mistaking an incomplete graph for a complete one. A pure, deterministic function of the graph (renderer drift never reaches a gate), and like the JSON it is a **view, never gated**. `--root <entry>` scopes it to one handler's forward reach over the *unscoped* graph (keeping the frontier markers a build-time `--entry` scope would drop); `--diff <base.graph.json>` colors the base→branch delta (added/removed nodes and edges), the visual form of the comparison `groundwork review` performs — still a view, never the verdict.
- **Upstream/downstream:** for any function, its callers (`In`) and callees (`Out`) — the relationship map you asked for at the outset.
- **Per-entry-point subgraphs** — mirror the behavioral flows; the `--mermaid --root <entry>` flowchart is their rendered form.
- The structured IR **is** the AI-readable relationship map (free), and backs a future queryable interface.
- **Identity seam with the behavioral pipeline (deliberate):** static nodes are keyed by FQN; behavioral nodes by canonical `Op`. They are *not* joined at arbitrary functions — they join at **entry points** (the shared roots) and at **event names** (the bus contract appears in both vocabularies), which is where joining matters. If function-level linkage is ever needed, the detail sidecar's positions are the bridge.
- **Overlap with the behavioral gate is benign.** A new published event on a *tested* path trips both this boundary gate (statically) and the behavioral snapshot (at runtime) — a consistent signal from two angles, not a contradiction. Consolidate it in the MR presentation if it reads as redundant; low priority.

---

## 10. Resolved decisions

Settled toward flexibility (the tool must cater to all repo and interface shapes), each as a default plus an opt-in:

- **Algorithm** → **RTA default** (fast, scales in CI, blind-spot manifest absorbs its imprecision); **VTA opt-in** for interface-dense repos or a slower high-fidelity pass; CHA fallback for rootless libraries.
- **Library root strategy** → **all-exported-symbols default** (the exported API is the entry surface; zero-config); optional narrowing to a declared public-API subset.
- **Monorepo-internal modules** → **traverse first-party siblings** (auto-detected from go.work/go.mod), bus stays the boundary; **per-service analysis unit** as the opt-in for very large monorepos.
- **Detail sidecar** → **ship positions** (keyed to node/edge IDs, regenerated, never gated; valuable for the AI-assist consumer), **opt-out** for teams wanting a minimal surface.
