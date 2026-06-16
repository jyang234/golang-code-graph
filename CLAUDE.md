# golang-code-graph

A determinism- and trust-critical toolchain. It turns captured behavior and static
structure into a **canonical IR** and emits **verdicts** — PROVEN / VIOLATED /
CANT-PROVE, snapshot diffs, gate pass/fail — that humans and CI act on without
re-deriving them. The product *is* the trustworthiness of those verdicts.

## Prime directive

**Determinism and trust come before everything else — features, speed, brevity,
convenience. No exceptions.** The worst outcome in this codebase is never a crash;
it is a *confidently wrong, silent* result: a non-deterministic "canonical" form, a
false SATISFIED, a hidden disclosure, a verdict that changes run-to-run. When any
other goal trades against determinism or trust, the other goal loses. If you cannot
make a change both correct and trustworthy, make it abstain.

## Core tenets

1. **Determinism is the whole point.** Output is a pure function of its inputs —
   byte-identical across repeated runs. Anything that can vary run-to-run (wall
   clock, ids, map iteration order, the race outcome of concurrent siblings) is
   discarded or canonicalized, never allowed to reach output. This is what an
   LLM judge cannot offer and what lets the result be a hard CI gate.

2. **Fail closed.** An incomplete, ambiguous, or unverifiable input is refused, not
   guessed. A nil/empty/abstain result that the caller surfaces beats a plausible
   wrong one. Prefer CANT-PROVE / UNKNOWN over a fabricated pole.

3. **Self-honesty about blind spots.** Where the analysis cannot see (closures,
   dynamic dispatch, non-constant arguments), say so explicitly and route it to the
   blind-spot / `trust: verify` channel — never launder an unknown into a concrete
   claim. A check that is silently wrong even occasionally gets muted and abandoned;
   the honesty about its own limits is what keeps it trusted.

4. **Soundness is asymmetric.** A proof (ALWAYS, "covered") may only ever be emitted
   when actually proven. A negative (NEVER, "no path") may follow only from
   over-approximated reachability, and holds only *outside* the blind spots.
   Everything unprovable is UNKNOWN — never silently treated as either pole.

5. **The machine is not the oracle.** The human is the only judge of *intent*.
   Everything a machine can verify must be verified mechanically (regenerate-and-
   diff, replay-and-compare, fuzz-and-crash). Never add a gate that relies on a
   heuristic or AI judgment of correctness — make the invariant self-checking
   instead. The author of a change must not be the sole grader of it.

## Coding standards

### Determinism in practice
- Every ordering and tie-break resolves on **intrinsic, run-independent data** (a
  canonical key, a content signature) — never arrival order, start time, or map
  iteration. When you add a sort, ask "what happens on a tie?" and break it
  deterministically.
- New ordering or canonicalization paths ship with a **determinism test or an
  extension to the canon fuzz corpus** (`internal/canon`:
  `TestConcurrentSameOpDeterministicOrder`, `FuzzCanonConcurrentOrderInvariant`).

### One source of truth
- A rule applied in two places (e.g. the write-verb set in `fitness.IsWrite` and
  `graphio.mutatingSQLOp`) lives in one helper or constant, and the parity is named
  in a comment **and** guarded by a test. Do not copy a predicate and let the copies
  drift.

### Comments are load-bearing — keep them honest
Comments here encode **intent and invariants**, not narration; they are the oracle a
reviewer uses to tell a bug from a feature. A stale comment misleads everyone,
including whoever "fixes" code to match a claim that was never true.
1. **When you change a function body, re-read its doc and inline invariant comments.**
   If the change makes a *checkable* claim false (*always / never / sorted /
   deterministic / byte-identical / X before Y / cannot / exactly one*), update the
   comment in the same edit or call it out. Code and the comment above it must not
   disagree in a diff you author.
2. **Comment the WHY, pin the WHAT with a test.** Keep prose that explains *why* a
   tie-break or abstain branch exists. Back a *checkable* assertion with a test so it
   is enforced, not just stated. Delete pure restatement (`i++ // increment i`).

The `commentdrift` nudge flags the common case (a body that moved under an unchanged
asserting comment). Install it once with `make hooks`; run ad hoc with
`make comment-drift`. It is advisory (exits 0) unless `COMMENTDRIFT_STRICT=1`.

### Style
- `gofmt`, `go vet`, and `golangci-lint` must be clean — CI mirrors them exactly.
- Match the surrounding idiom, naming, and comment density. Prefer small, focused
  functions; guard nil/empty paths explicitly.

## Build / verify
- `make verify` — build + vet + lint + test + fixture + gofmt; **must be green at the
  end of every change.**
- `make fmt-check` — fast gofmt gate.
- `make cover-floor` — non-regressing coverage floor for the static front-end.
- `make hooks` — install the comment-drift pre-commit nudge (once per clone).
