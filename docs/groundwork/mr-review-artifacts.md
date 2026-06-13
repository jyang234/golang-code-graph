# Deterministic MR review artifacts

> **`DESIGN RECORD`** · shipped; kept for rationale · _reviewed 2026-06-13_

## The problem in 100%-agent development

When Claude Code writes 100% of the code, the bottleneck moves to the human
reviewer — and the reviewer has lost the passive comprehension they used to get
from writing the code themselves. Every MR arrives as a stranger, and they must
reconstruct *what moved and what it affects* from the diff before they can judge
the thing only a human can judge: **is the intent right.**

The trap: the artifacts the agent produces to explain its change — the MR
description, the summary — are authored by the same system under review. They can
be confidently wrong or paper over a problem. You cannot solve an
AI-comprehension problem with more AI-generated prose.

## The mechanism

A review artifact computed *from* the call graphs of base vs branch, not *about*
them. The agent cannot embellish it because it is a function of the code's actual
structure. It answers, in reviewer priority order:

1. **Verdict** — BLOCK or CLEAR (any new invariant violation or breaking contract ⇒ BLOCK).
2. **Shape** — body-only / localized / cross-package / broad. The reviewer's first triage.
3. **New invariant violations** — layering, reachability, budget — each naming the exact edge.
4. **Contract movement** — routes/events added or removed, breaking flagged.
5. **External I/O effects** — DB writes, bus publish/consume the MR adds or removes.
6. **Reach** — which existing entrypoints the change is now live behind.

```bash
groundwork review policy.json base-graph.json branch-graph.json \
    --base-contract base.json --head-contract branch.json
```

## The anti-fake guarantee

Every artifact carries `digest = sha256(structural content)`. Two independent
checks prove it cannot be faked:

```bash
groundwork verify-artifact artifact.json policy.json base-graph.json branch-graph.json ...
```

1. **Body integrity** — the claimed body must hash to the claimed digest. Edit
   any field (e.g. flip BLOCK→CLEAR) and the hash no longer matches → **TAMPERED**.
2. **Code correspondence** — the claimed digest must equal the digest recomputed
   from the real base/branch graphs. Point it at different code → **STALE**.

Demonstrated: an artifact whose verdict was hand-edited from BLOCK to CLEAR is
caught — `hash of claimed body` diverges from `claimed digest`, exit 1. The agent
cannot pre-compute a favorable digest because the inputs are the actual graphs,
and cannot edit the rendered report because that breaks body integrity.

## What it looks like (real output)

A feature MR — "add publisher health endpoint" — that *also* quietly wires the
handler straight to storage, bypassing the domain layer (the kind of thing lost
in a 600-line diff). The MR description reads benign; the artifact reads:

```text
# MR structural review — BLOCK
digest 0d40d53f84f2ba46 · recompute to verify (deterministic; not author-editable)
substrate: vta — vta refined over rta from 6 discovered root(s)

Shape: cross-package
Touches: app(+3), handler(+3), storage(+2)

⛔ Introduces 1 invariant violation(s)
- layering — handler -> storage skips 1 layer(s)
  - handler.Server.GetPublisherHealth -> storage.PostgresStore.Publishers

🔌 External contract changed (additive)
- + route GET /v1/publishers/{publisherId}/health

🌐 Reachable from 2 existing entrypoint(s)
```

The same feature done correctly (handler → app → storage) renders **CLEAR** with
no violations. The reviewer spends ~15 seconds confirming "structurally this is
what it claims," then spends their real attention on intent — instead of burning
it reconstructing the change.

## How it changes the review economy

- **Triage in seconds:** the shape label + verdict tells the reviewer how hard to
  look before they open the diff.
- **Attention goes where only humans help:** the artifact handles "what moved and
  what it affects"; the human handles "is this the right thing to build" and the
  sins of omission a graph can't see.
- **Trust the green, deterministically:** a CLEAR verdict is *computed*, not
  asserted by the author. The reviewer (or CI) can recompute the digest, so a
  pass means the same thing every time, for every agent, regardless of how the MR
  was written.

## The boundary (unchanged)

This artifact certifies **structural** facts: what moved, what invariants held,
what the contract did, what I/O changed, what's reachable. It does not certify
the logic inside a function is correct — that stays tests and types, and the
reviewer still judges intent. Its job is to remove the *comprehension tax* of
agent-authored change and to make the structural claims **unfakeable**, so the
human's scarce attention lands on the questions a graph can't answer.
