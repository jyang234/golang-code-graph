# CX-5 inputs — the two human answers that unblock chain cards

**To:** the groundwork deployment team (Koalafi Shared Tech)
**Why:** CX-5 (cross-service effect chains) is the last unbuilt phase. It is
*observational-first* and rests on two things a tool cannot compute — a broker
guarantee you stand behind, and which facts actually mattered in a real
incident. Both are pre-filled below from your 2026-06-13 field report; we need
you to **confirm, correct, or sign**, not author from scratch.

---

## Input 1 — broker delivery, signed

A chain card labels every link **proven** (computed from a service's graph) or
**assumed** (a broker behavior no static analysis can establish). The assumed
links are printed verbatim from a policy declaration and **never inferred** —
so the card needs a guarantee with a name on it, not our guess.

From your report, the system is SNS→SQS standard queues: at-least-once,
unordered, idempotent consumers (inbox dedup on `UNIQUE(source_id, topic)`).
Encoded as the policy block the card will print:

```jsonc
"brokers": {
  "bus": {
    "transport":   "sns->sqs (standard queue)",
    "delivery":    "at-least-once",   // at-least-once | at-most-once | exactly-once
    "ordered":     false,             // false | per-key:<key> | total
    "consumers":   "idempotent",      // idempotent | not-idempotent
    "dedup":       "inbox UNIQUE(source_id, topic)"
  }
}
```

**What we need (one of):**
- [ ] *Confirm as-is* — sign-off: name + date below, and this becomes the
  declaration every chain card prints.
- [ ] *Correct a field* — change any value above; the rest stand.
- [ ] *Split by topic* — if a specific topic (e.g. `loan.approved`) has a
  stronger or weaker guarantee than the bus default, name it and its values.

The one question to settle each field: **would you defend this value to a
responder reading it on a fault card at 3am?** If not, weaken it until you
would — an honest "unordered" beats an aspirational "ordered."

> Signed: ______________________  Date: __________

---

## Input 2 — which effect_order facts earned their place

The derived facts are true by construction; the question is which ones a
responder *reached for*. Your answer (a) grades whether the coverage answers
real questions (E-CX1) and (b) picks the **first chain card we build** — we
render the one that has paid off in a live incident, not a fixture-chosen one.

You named two hot-path candidates. For each, the minimum we need:

| candidate fact | cited in a real incident? | the question the responder needed answered |
|---|---|---|
| `provisioning_outbox` DELETE ordering (domain row + outbox row in one tx) | ☐ yes ☐ no | e.g. "were outbox rows orphaned after the domain row was deleted?" → *your words* |
| dual publish in `publishWithFanout` (version topic then template topic) | ☐ yes ☐ no | e.g. "the fault hit between the two publishes — which side-effects already happened?" → *your words* |

And two open prompts (one line each is plenty):

1. **Any *other* effect_order fact** — beyond these two — that a responder used
   in the last few incidents? (If none, that itself is the answer: the two
   above are the whole hot path.)
2. **The most recent incident where "what already committed?" was the live
   question.** Just the shape — route or symptom, and which fact (if any)
   would have answered it. This becomes the chain card's worked example.

---

## What happens with your answers

- Input 1 → the broker block lands in policy; every chain card prints it as the
  *assumed* half of the happens-before chain, with your sign-off as its
  warrant.
- Input 2 → we build the cross-service card for the cited fact first (likely
  the outbox or the dual publish), with your incident as its acceptance
  scenario. If neither was ever cited, that is a real signal too — CX-5 parks
  (the E-CX5 ROI gate) rather than shipping a surface no one reached for.

Neither answer needs to be long. The broker sign-off is a name on a block you
already described; the incident input is two checkboxes and a sentence. Send
whatever you have — partial is fine, and "never came up in an incident" is a
valid, useful answer to either half.
