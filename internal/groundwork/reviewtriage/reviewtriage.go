// Package reviewtriage is a PROTOTYPE human-reviewer triage surface: given the
// base and branch graphs of an MR, it partitions the *changed* functions into two
// zones for a human reviewer drowning in diff volume —
//
//   - VOUCHED: every path through the change is statically resolved, so the tool
//     can show the COMPLETE evidence (which entrypoints it is live behind, the exact
//     boundary-effect surface it can reach) and the reviewer can verify that evidence
//     against the code rather than re-derive it. "Don't take my word for it — here is
//     the proof, go check it."
//   - FOCUS: the change touches or sits behind a disclosed blind spot (dynamic
//     dispatch, reflection, non-constant I/O, an over-approximated seam), so the tool
//     CANNOT give complete evidence. These are exactly where a reviewer's scarce
//     attention should go — both because the tool can't vouch and because a blind
//     spot is precisely where a hallucinated or poisoned understanding can hide.
//
// This serves the two founding goals directly: (a) combat hallucination/context
// poisoning by being a deterministic reference frame whose incompleteness is LOUD,
// never silent — a silently-incomplete map would falsely corroborate a lie living in
// its blind spot; (b) route a reviewer's verification effort by the inverse of the
// tool's own confidence. It is pure composition over the existing graph index and the
// impact evidence engine — a pure function of (base, branch), no policy, no verdict.
//
// PROTOTYPE scope/limits (ride with the report): the changed set is the set-based
// node/edge/effect delta (a new call *site* to an already-called target is not a new
// edge — same limit as review's delta); the per-function evidence is a static blast
// radius (what the change COULD touch, not the route a given input takes); and a
// VOUCHED change is vouched for STRUCTURE only — the tool never judges whether the
// resolved effects are the *right* ones (that is the reviewer's, and the policy's, job).
package reviewtriage

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/impact"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
)

// ChangedFn is one changed function with its evidence card and, when the tool
// cannot fully vouch, the reasons why (empty ⇒ vouched). Deterministic: every field
// is sorted, so identical (base, branch) render identically.
type ChangedFn struct {
	FQN     string      `json:"fqn"`
	Card    impact.Card `json:"card"`              // the evidence: entrypoint cover, reachable effects, blind spots
	Reasons []string    `json:"reasons,omitempty"` // why it needs human eyes; empty when fully resolved (vouched)
}

// Report is the two-zone triage of an MR's changed functions.
type Report struct {
	BaseNodes   int         `json:"base_nodes"`
	BranchNodes int         `json:"branch_nodes"`
	Vouched     []ChangedFn `json:"vouched,omitempty"` // fully resolved — complete evidence shown
	Focus       []ChangedFn `json:"focus,omitempty"`   // touches a blind spot — evidence incomplete, look here
}

// Build computes the triage. The changed set is the union of: functions new in the
// branch, functions whose signature changed, and functions that gained an outgoing
// internal call or boundary effect — i.e. the functions whose structure or effect
// surface the MR moved. Each is run through the impact evidence engine over the
// BRANCH graph (the post-merge reality the reviewer is judging); a change with any
// blind spot or over-approximation on a traversed path lands in Focus, the rest in
// Vouched. Deterministic: the changed set and every card field are sorted.
func Build(base, branch *graph.Graph) Report {
	ix := graph.NewIndex(branch)
	var vouched, focus []ChangedFn
	for _, fqn := range changedFns(base, branch) {
		card := impact.ForNodes(ix, []string{fqn})
		cf := ChangedFn{FQN: fqn, Card: card, Reasons: focusReasons(card)}
		if len(cf.Reasons) == 0 {
			vouched = append(vouched, cf)
		} else {
			focus = append(focus, cf)
		}
	}
	return Report{
		BaseNodes:   len(base.Nodes),
		BranchNodes: len(branch.Nodes),
		Vouched:     vouched,
		Focus:       focus,
	}
}

// changedFns is the sorted set of branch functions the MR structurally moved.
func changedFns(base, branch *graph.Graph) []string {
	baseSig := make(map[string]string, len(base.Nodes))
	for _, n := range base.Nodes {
		baseSig[n.FQN] = n.Sig
	}
	branchNode := make(map[string]bool, len(branch.Nodes))
	for _, n := range branch.Nodes {
		branchNode[n.FQN] = true
	}
	changed := map[string]bool{}
	for _, n := range branch.Nodes {
		if old, existed := baseSig[n.FQN]; !existed || old != n.Sig {
			changed[n.FQN] = true // new function, or its signature moved
		}
	}
	baseEdge := make(map[string]bool, len(base.Edges))
	for _, e := range base.Edges {
		baseEdge[e.From+"\x00"+e.To] = true
	}
	for _, e := range branch.Edges {
		// A function that gained a callee or a boundary effect changed behavior, even
		// if its own node is unchanged. Restrict to a real branch node so a synthetic
		// boundary endpoint is never mistaken for a changed function.
		if branchNode[e.From] && !baseEdge[e.From+"\x00"+e.To] {
			changed[e.From] = true
		}
	}
	return setutil.SortedKeys(changed)
}

// focusReasons turns an evidence card's blind spots and over-approximations into the
// reviewer-actionable reasons the tool cannot fully vouch for the change. Empty ⇒
// every path is resolved ⇒ the change is vouched. Order follows the card's already-
// sorted blind spots, then the two over-approximation flags, so it is deterministic.
func focusReasons(c impact.Card) []string {
	var rs []string
	for _, b := range c.BlindSpots {
		rs = append(rs, blindReason(b))
	}
	if c.EffectsOverApprox {
		rs = append(rs, "the reachable-effect surface is an UPPER BOUND — the forward reach crossed a shared dispatch seam (HighFanOut), so it may include effects of sibling code, not just this change")
	}
	if c.CoverOverApprox {
		rs = append(rs, "the set of entrypoints this is live behind is an UPPER BOUND — the reverse reach crossed a shared dispatch seam (HighFanOut); confirm which routes actually reach it")
	}
	return rs
}

// blindReason renders one blind spot as a reviewer-actionable sentence: what the
// tool cannot see, where, and the implicit thing to verify. The phrasing is keyed on
// the disclosed Kind (the same vocabulary flowmap emits); an unrecognized kind falls
// back to an honest generic rather than dropping the disclosure (fail loud).
func blindReason(b graph.BlindSpot) string {
	at := b.Site
	if at == "" {
		at = "an undisclosed site"
	}
	detail := ""
	if b.Detail != "" {
		detail = " (" + b.Detail + ")"
	}
	switch b.Kind {
	case "NonConstantBoundaryArg":
		return fmt.Sprintf("a boundary call with a NON-CONSTANT target at %s%s — the tool cannot tell which destination; verify the value", at, detail)
	case "UnresolvedDispatch", "UnresolvedCall":
		return fmt.Sprintf("a call through a function value the tool cannot resolve at %s%s — the actual callee, and what it does, is invisible here; verify it", at, detail)
	case "ConcurrentDispatch":
		return fmt.Sprintf("an unresolved goroutine dispatch at %s%s — concurrent behavior past this point is invisible to the tool", at, detail)
	case "DynamicEffect":
		return fmt.Sprintf("a DYNAMIC boundary effect at %s%s — the tool sees that an effect happens but not its full identity", at, detail)
	case "HighFanOut":
		return fmt.Sprintf("a dispatch site fanning to many possible targets at %s%s — the tool over-approximates here; confirm which target this change intends", at, detail)
	case "reflect":
		return fmt.Sprintf("reflection at %s%s — call structure here is invisible to static analysis", at, detail)
	case "unsafe", "cgo", "go:linkname":
		return fmt.Sprintf("%s at %s%s — bypasses the analyzable call graph", b.Kind, at, detail)
	case "ExternalBoundaryCall":
		return fmt.Sprintf("a call into a third-party package at %s%s — the tool cannot see inside it", at, detail)
	case "ImpeachmentSeam":
		return fmt.Sprintf("a behaviorally-proven blind spot at %s%s — runtime has shown this seam hides effects", at, detail)
	default:
		return fmt.Sprintf("%s at %s%s — the tool's view stops here", b.Kind, at, detail)
	}
}

// RenderMarkdown is the human-reviewer report: focus zone first (where attention is
// scarce, lead with where it must go), then the vouched zone with its checkable
// evidence. A change with no structural movement yields an explicit "nothing to
// triage" rather than an empty page (silence is never a silent pass).
func (r Report) RenderMarkdown() string {
	var b strings.Builder
	v, f := len(r.Vouched), len(r.Focus)
	fmt.Fprintf(&b, "# MR review triage — where to spend your verification\n")
	fmt.Fprintf(&b, "_graph %d → %d nodes · %d changed function(s): %d need your eyes, %d the tool can vouch for_\n",
		r.BaseNodes, r.BranchNodes, v+f, f, v)

	if v+f == 0 {
		b.WriteString("\nNo structural change detected (body-only or no diff). The tool has nothing to triage here — that is not the same as \"safe\"; it means the change did not move the call graph, so verify behavior the usual way.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "\n## ⚠️  Focus here — %d change(s) the tool CANNOT vouch for\n", f)
	if f == 0 {
		b.WriteString("_None — every changed path is statically resolved. Still your call, but the tool has no blind spot to flag._\n")
	}
	for _, cf := range r.Focus {
		fmt.Fprintf(&b, "\n### %s\n", cf.FQN)
		b.WriteString("The tool cannot give you complete evidence here:\n")
		for _, reason := range cf.Reasons {
			fmt.Fprintf(&b, "- %s\n", reason)
		}
		writeEvidence(&b, cf.Card, true)
	}

	fmt.Fprintf(&b, "\n## ✅ Vouched — %d change(s), fully resolved (check the evidence, don't take the tool's word)\n", v)
	if v == 0 {
		b.WriteString("_None — every changed function touches a blind spot above._\n")
	}
	for _, cf := range r.Vouched {
		fmt.Fprintf(&b, "\n### %s\n", cf.FQN)
		b.WriteString("Every path through this change is statically resolved — no dynamic dispatch, reflection, or opaque I/O on any reachable path. Evidence to verify against the code:\n")
		writeEvidence(&b, cf.Card, false)
	}

	b.WriteString("\n---\n")
	b.WriteString("_Triage is the static MAP (what each change COULD touch), not the route a given input takes; and a vouched change is vouched for STRUCTURE only — whether the resolved effects are the RIGHT ones is your call. PROTOTYPE._\n")
	return b.String()
}

// writeEvidence prints the checkable facts of a change: the entrypoints it is live
// behind and the boundary-effect surface it reaches. partial marks the focus-zone
// case, where the same facts are shown but flagged as incomplete (a blind spot may
// hide more), so the reviewer reads them as a floor, not the whole truth.
func writeEvidence(b *strings.Builder, c impact.Card, partial bool) {
	qualifier := ""
	if partial {
		qualifier = " (a FLOOR — the blind spot(s) above may hide more)"
	}

	cover := c.Entrypoints
	coverNote := ""
	if c.CoverOverApprox {
		coverNote = " ≤ (upper bound)"
	}
	if len(cover) == 0 {
		fmt.Fprintf(b, "- live behind no discovered entrypoint%s\n", qualifier)
	} else {
		fmt.Fprintf(b, "- live behind %d entrypoint(s)%s%s:\n", len(cover), coverNote, qualifier)
		for _, e := range cover {
			fmt.Fprintf(b, "  - %s\n", e)
		}
	}

	effects := trimmedEffects(c.Effects)
	switch {
	case len(effects) == 0 && !partial:
		b.WriteString("- reaches NO boundary effects — a pure/internal change (no DB, bus, or outbound I/O on any path)\n")
	case len(effects) == 0:
		b.WriteString("- no boundary effect resolved on the visible paths\n")
	default:
		surface := "the COMPLETE boundary-effect surface of this change"
		if partial {
			surface = "the boundary effects the tool CAN see"
		}
		fmt.Fprintf(b, "- reaches %d boundary effect(s) — %s%s:\n", len(effects), surface, qualifier)
		for _, e := range effects {
			fmt.Fprintf(b, "  - %s\n", e)
		}
	}
}

// trimmedEffects strips the internal "boundary:" prefix from each effect label for
// display — the same human-readable form the ground/triage cards use — and keeps
// them sorted.
func trimmedEffects(effects []string) []string {
	out := make([]string, 0, len(effects))
	for _, e := range effects {
		out = append(out, strings.TrimPrefix(e, "boundary:"))
	}
	sort.Strings(out)
	return out
}
