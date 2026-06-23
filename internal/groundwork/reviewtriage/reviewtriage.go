// Package reviewtriage is a PROTOTYPE human-reviewer triage surface: given the base
// and branch graphs of an MR, it sorts the *changed* functions into three zones for a
// reviewer drowning in diff volume, by the inverse of the tool's own confidence AND by
// what THIS diff actually moved —
//
//   - NEW BLIND (focus): the change introduces or newly reaches a blind spot — the
//     tool couldn't see here before, and the change now routes into the blindness. This
//     MR made something newly unverifiable; flag it loud.
//   - CARRIED BLIND: the change is resolved at its own level, but its effect surface
//     passes through a blind spot that ALREADY existed on this path. Not introduced
//     here — so it doesn't dominate — but disclosed, never background.
//   - ACCOUNTED: the forward cone is fully resolved, so the tool can show the COMPLETE
//     evidence (entrypoint cover, exact effect surface) for the reviewer to check.
//     This is NOT "approved": the tool vouches for STRUCTURE, not for correctness or
//     intent — the reviewer still verifies the resolved effects are the right ones. The
//     tool accepts nothing at face value; "accounted" only means "nothing here is
//     hidden from you."
//
// This serves the two founding goals: (a) combat hallucination/context poisoning by
// being a deterministic reference frame whose incompleteness is LOUD — and, for a diff,
// whose NEWLY-incomplete regions are loudest, because that is where a poisoned
// understanding most easily slips in unreviewed; (b) route a reviewer's verification
// effort by confidence × novelty. Pure composition over the graph index and the impact
// evidence engine — a pure function of (base, branch), no policy, no verdict.
//
// PROTOTYPE scope/limits (ride with the report): the changed set is the set-based
// node/edge/effect delta; the per-function evidence is a static blast radius (what the
// change COULD touch, not the route a given input takes); novelty is computed by
// comparing each function's base vs branch FORWARD blind-spot set, so a brand-new call
// SITE to an already-reachable blind spot reads as carried, not new (the set-based
// limit); and "accounted" is structural completeness, never approval.
package reviewtriage

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/impact"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
	"github.com/jyang234/golang-code-graph/internal/sqlverb"
)

// ChangedFn is one changed function with its evidence and the forward blind spots that
// keep the tool from fully accounting for it, split by whether THIS MR introduced them.
// Deterministic: every field derives from sorted graph data.
type ChangedFn struct {
	FQN  string `json:"fqn"`
	Tier int    `json:"tier,omitempty"`

	// Evidence — the facts a reviewer can check against the code.
	Entrypoints     []string `json:"entrypoints,omitempty"`       // reverse-reach cover: the routes it is live behind
	CoverUpperBound bool     `json:"cover_upper_bound,omitempty"` // the cover crossed a reverse HighFanOut seam — context, not a zone reason (#1)
	Effects         []string `json:"effects,omitempty"`           // forward boundary effects it can reach (human-readable)

	// NewSeams are serious forward blind spots this MR introduced or newly reaches;
	// CarriedSeams pre-existed on this path. The split is what separates the focus zone
	// (new) from the carried zone (disclosed, not new).
	NewSeams     []graph.BlindSpot `json:"new_seams,omitempty"`
	CarriedSeams []graph.BlindSpot `json:"carried_seams,omitempty"`
	BenignSeams  []string          `json:"benign_seams,omitempty"` // trivial-severity seams, set aside but disclosed (#2)

	NewOverApprox     bool `json:"new_over_approx,omitempty"`     // a forward HighFanOut over-approximation introduced by this MR
	CarriedOverApprox bool `json:"carried_over_approx,omitempty"` // a forward HighFanOut that pre-existed
}

// Report is the three-zone triage of an MR's changed functions, ordered by descending
// attention: new blindness, then carried blindness, then the fully-accounted rest.
type Report struct {
	BaseNodes   int         `json:"base_nodes"`
	BranchNodes int         `json:"branch_nodes"`
	NewBlind    []ChangedFn `json:"new_blind,omitempty"`
	Carried     []ChangedFn `json:"carried,omitempty"`
	Accounted   []ChangedFn `json:"accounted,omitempty"`
}

// Build computes the triage over the BRANCH graph (the post-merge reality the reviewer
// is judging). For each changed function it compares the branch forward blind-spot set
// against the BASE one to tell new blindness from carried (the diff-delta that keeps a
// pre-existing blind spot from reading as this MR's fault, and a new one from blending
// into the background). The blind zones are consequence-ranked (#4).
func Build(base, branch *graph.Graph) Report {
	branchIx, baseIx := graph.NewIndex(branch), graph.NewIndex(base)
	baseNode := nodeSet(base)
	tier := tierLookup(branch)
	var newBlind, carried, accounted []ChangedFn
	for _, fqn := range changedFns(base, branch) {
		card := impact.ForNodes(branchIx, []string{fqn})                             // evidence: reverse cover + forward effects
		branchBlind, branchOver := impact.ForwardBlindSpots(branchIx, []string{fqn}) // forward-only (#1)
		branchSerious, benign := splitSeverity(branchBlind)                          // set aside benign seams (#2)

		// Base forward state for the SAME function — empty for a function new in this MR
		// (so all its blindness is, correctly, new).
		var baseSerious []graph.BlindSpot
		baseOver := false
		if baseNode[fqn] {
			pb, po := impact.ForwardBlindSpots(baseIx, []string{fqn})
			baseSerious, _ = splitSeverity(pb)
			baseOver = po
		}
		newSeams, carriedSeams := splitNewCarried(branchSerious, baseSerious)

		cf := ChangedFn{
			FQN:               fqn,
			Tier:              tier[fqn],
			Entrypoints:       card.Entrypoints,
			CoverUpperBound:   card.CoverOverApprox,
			Effects:           trimmedEffects(card.Effects),
			NewSeams:          newSeams,
			CarriedSeams:      carriedSeams,
			BenignSeams:       benignNotes(benign),
			NewOverApprox:     branchOver && !baseOver,
			CarriedOverApprox: branchOver && baseOver,
		}
		switch {
		case len(cf.NewSeams) > 0 || cf.NewOverApprox:
			newBlind = append(newBlind, cf) // this MR made it unverifiable — focus
		case len(cf.CarriedSeams) > 0 || cf.CarriedOverApprox:
			carried = append(carried, cf) // blind, but not newly so — disclosed, not background
		default:
			accounted = append(accounted, cf) // complete evidence — NOT approval
		}
	}
	sortByConsequence(newBlind)
	sortByConsequence(carried)
	// accounted keeps changedFns' FQN order (the low-attention zone).
	return Report{
		BaseNodes:   len(base.Nodes),
		BranchNodes: len(branch.Nodes),
		NewBlind:    newBlind,
		Carried:     carried,
		Accounted:   accounted,
	}
}

// splitNewCarried partitions the branch's serious forward blind spots into those NOT
// present in the base forward set (new — this MR introduced or newly reaches them) and
// those present in both (carried). The seam identity is (Kind, Site, Detail), the same
// key the impact engine dedups on, so a blind spot newly REACHED via an added edge (its
// site existed but was unreachable from this function in the base) is correctly new.
func splitNewCarried(branchSerious, baseSerious []graph.BlindSpot) (newSeams, carried []graph.BlindSpot) {
	had := map[string]bool{}
	for _, b := range baseSerious {
		had[seamKey(b)] = true
	}
	for _, b := range branchSerious {
		if had[seamKey(b)] {
			carried = append(carried, b)
		} else {
			newSeams = append(newSeams, b)
		}
	}
	return newSeams, carried
}

func seamKey(b graph.BlindSpot) string { return b.Kind + "\x00" + b.Site + "\x00" + b.Detail }

// changedFns is the sorted set of branch functions the MR structurally moved: new
// functions, signature changes, and functions that gained an outgoing call or effect.
func changedFns(base, branch *graph.Graph) []string {
	baseSig := make(map[string]string, len(base.Nodes))
	for _, n := range base.Nodes {
		baseSig[n.FQN] = n.Sig
	}
	branchNode := nodeSet(branch)
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

// splitSeverity divides forward-cone blind spots into the zone-worthy (serious) and the
// producer-tagged-benign (trivial). Only Severity=="trivial" is benign; every other
// value — including the empty default that covers reflection, dynamic effects, and
// unresolved dispatch — is serious (#2 fails toward flagging, never hiding).
func splitSeverity(bs []graph.BlindSpot) (serious, benign []graph.BlindSpot) {
	for _, b := range bs {
		if b.Severity == "trivial" {
			benign = append(benign, b)
		} else {
			serious = append(serious, b)
		}
	}
	return serious, benign
}

// benignNotes renders the set-aside trivial seams, so an accounted change with a benign
// seam never claims a completeness it does not have.
func benignNotes(benign []graph.BlindSpot) []string {
	var out []string
	for _, b := range benign {
		site := b.Site
		if site == "" {
			site = "an undisclosed site"
		}
		out = append(out, fmt.Sprintf("%s at %s — producer-tagged trivial (a benign seam, e.g. a cancel-func dispatch)", b.Kind, site))
	}
	return out
}

// seamReasons renders blind spots as reviewer-actionable sentences.
func seamReasons(seams []graph.BlindSpot) []string {
	rs := make([]string, 0, len(seams))
	for _, b := range seams {
		rs = append(rs, blindReason(b))
	}
	return rs
}

// sortByConsequence orders a blind zone so scarce reviewer attention lands on the most
// consequential change first (#4): most-critical salience tier, then a change that can
// MUTATE state over a read-only one, then the larger blast radius, then FQN.
func sortByConsequence(fs []ChangedFn) {
	sort.SliceStable(fs, func(i, j int) bool {
		if a, b := tierRank(fs[i].Tier), tierRank(fs[j].Tier); a != b {
			return a < b
		}
		if a, b := reachesMutating(fs[i].Effects), reachesMutating(fs[j].Effects); a != b {
			return a
		}
		if a, b := len(fs[i].Entrypoints), len(fs[j].Entrypoints); a != b {
			return a > b
		}
		return fs[i].FQN < fs[j].FQN
	})
}

// tierRank orders salience tiers most-critical-first and sends the unset tier (0) to the
// back, so a real tier always outranks "unknown".
func tierRank(t int) int {
	if t <= 0 {
		return 1 << 30
	}
	return t
}

// reachesMutating is a RANKING-ONLY heuristic (no verdict rests on it): does the change's
// resolved effect surface include a write — a mutating SQL verb via the one shared
// sqlverb source, or a bus PUBLISH?
func reachesMutating(effects []string) bool {
	for _, e := range effects {
		if f := strings.Fields(e); len(f) >= 2 && f[0] == "db" && sqlverb.Mutating(f[1]) {
			return true
		}
		if strings.Contains(e, "PUBLISH") {
			return true
		}
	}
	return false
}

// blindReason renders one blind spot as a reviewer-actionable sentence: what the tool
// cannot see, where, and the implicit thing to verify. Keyed on the disclosed Kind; an
// unrecognized kind falls back to an honest generic rather than dropping the disclosure.
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

// RenderMarkdown is the human-reviewer report: new blindness first (where this diff most
// needs eyes), then carried blindness (disclosed, not new), then the fully-accounted
// rest (complete evidence — explicitly NOT approval). A change with no structural
// movement yields an explicit "nothing to triage" rather than an empty page.
func (r Report) RenderMarkdown() string {
	var b strings.Builder
	n, c, a := len(r.NewBlind), len(r.Carried), len(r.Accounted)
	fmt.Fprintf(&b, "# MR review triage — where to spend your verification\n")
	fmt.Fprintf(&b, "_graph %d → %d nodes · %d changed function(s): %d NEW blind, %d carried blind, %d fully accounted_\n",
		r.BaseNodes, r.BranchNodes, n+c+a, n, c, a)

	if n+c+a == 0 {
		b.WriteString("\nNo structural change detected (body-only or no diff). The tool has nothing to triage here — that is not the same as \"safe\"; it means the change did not move the call graph, so verify behavior the usual way.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "\n## ⚠️  New blindness — %d change(s) this diff makes newly unverifiable (focus here)\n", n)
	if n > 0 {
		b.WriteString("_ordered by consequence: salience tier, then state-mutating, then blast radius_\n")
	} else {
		b.WriteString("_None — this diff introduces no new blind spot. (Pre-existing blindness, if any, is below.)_\n")
	}
	for _, cf := range r.NewBlind {
		fmt.Fprintf(&b, "\n### %s%s\n", cf.FQN, tierTag(cf.Tier))
		b.WriteString("This change makes new paths unverifiable — the tool could not see here before, and the change now routes into the blindness:\n")
		for _, reason := range seamReasons(cf.NewSeams) {
			fmt.Fprintf(&b, "- %s\n", reason)
		}
		if cf.NewOverApprox {
			b.WriteString("- the reachable-effect surface became an UPPER BOUND — the change's forward reach newly crosses a shared dispatch seam (HighFanOut)\n")
		}
		if len(cf.CarriedSeams) > 0 || cf.CarriedOverApprox {
			fmt.Fprintf(&b, "- (it also passes through pre-existing blindness: %s)\n", strings.Join(distinctKinds(cf.CarriedSeams), ", "))
		}
		writeEvidence(&b, cf, true)
	}

	fmt.Fprintf(&b, "\n## 🟡 Carried blindness — %d change(s) resolved here, but on an already-partly-blind path (disclosed, not new)\n", c)
	if c == 0 {
		b.WriteString("_None._\n")
	}
	for _, cf := range r.Carried {
		fmt.Fprintf(&b, "\n### %s%s\n", cf.FQN, tierTag(cf.Tier))
		b.WriteString("Resolved at its own level, but its effect surface passes through a blind spot that ALREADY existed — not introduced by this change. Flagged so it does not blend into the background:\n")
		for _, reason := range seamReasons(cf.CarriedSeams) {
			fmt.Fprintf(&b, "- %s\n", reason)
		}
		if cf.CarriedOverApprox {
			b.WriteString("- the reachable-effect surface is an UPPER BOUND through a pre-existing shared dispatch seam (HighFanOut)\n")
		}
		writeEvidence(&b, cf, true)
	}

	fmt.Fprintf(&b, "\n## ✅ Fully accounted — %d change(s): complete evidence shown\n", a)
	b.WriteString("_The tool can show the COMPLETE structural surface for these. That is not approval — the tool accepts nothing at face value; verify the resolved effects are the ones you intend._\n")
	for _, cf := range r.Accounted {
		fmt.Fprintf(&b, "\n### %s%s\n", cf.FQN, tierTag(cf.Tier))
		if len(cf.BenignSeams) == 0 {
			b.WriteString("Every path through this change is statically resolved — no dynamic dispatch, reflection, or opaque I/O on any reachable path. Evidence to verify against the code:\n")
		} else {
			b.WriteString("Statically resolved except a benign seam the producer tagged trivial; the effect surface is otherwise complete. Evidence to verify against the code:\n")
			for _, s := range cf.BenignSeams {
				fmt.Fprintf(&b, "- (set aside) %s\n", s)
			}
		}
		writeEvidence(&b, cf, false)
	}

	b.WriteString("\n---\n")
	b.WriteString("_Triage is the static MAP (what each change COULD touch), not the route a given input takes; \"accounted\" is structural completeness, never approval. PROTOTYPE._\n")
	return b.String()
}

// writeEvidence prints the checkable facts of a change: the entrypoints it is live behind
// and the boundary-effect surface it reaches. partial marks a blind zone, where the same
// facts are a FLOOR (a blind spot may hide more).
func writeEvidence(b *strings.Builder, cf ChangedFn, partial bool) {
	floor := ""
	if partial {
		floor = " (a FLOOR — the blind spot(s) above may hide more)"
	}

	coverNote := ""
	if cf.CoverUpperBound {
		coverNote = " ≤ (upper bound — reverse dispatch seam)"
	}
	if len(cf.Entrypoints) == 0 {
		fmt.Fprintf(b, "- live behind no discovered entrypoint%s\n", floor)
	} else {
		fmt.Fprintf(b, "- live behind %d entrypoint(s)%s%s:\n", len(cf.Entrypoints), coverNote, floor)
		for _, e := range cf.Entrypoints {
			fmt.Fprintf(b, "  - %s\n", e)
		}
	}

	switch {
	case len(cf.Effects) == 0 && !partial:
		b.WriteString("- reaches NO boundary effects — a pure/internal change (no DB, bus, or outbound I/O on any path)\n")
	case len(cf.Effects) == 0:
		b.WriteString("- no boundary effect resolved on the visible paths\n")
	default:
		surface := "the COMPLETE boundary-effect surface of this change"
		if partial {
			surface = "the boundary effects the tool CAN see"
		}
		fmt.Fprintf(b, "- reaches %d boundary effect(s) — %s%s:\n", len(cf.Effects), surface, floor)
		for _, e := range cf.Effects {
			fmt.Fprintf(b, "  - %s\n", e)
		}
	}
}

// RenderMermaid draws the three-zone triage as a flowchart: changed functions colored by
// zone (new-blind = red, carried = amber, accounted = green), each blind change tied to a
// dashed seam node naming what the tool can't see past, and every change wired to the
// boundary effects it reaches — dashed from a blind change (a FLOOR), solid from an
// accounted one (the complete surface). Converging changes share an effect node.
// Deterministic: zones are ordered, effects emitted first-seen, all ids synthetic.
func (r Report) RenderMermaid() string {
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	b.WriteString("  classDef newblind fill:#fde8e8,stroke:#e02424,color:#771d1d;\n")
	b.WriteString("  classDef carried fill:#fff4e5,stroke:#d97706,color:#7c4a03;\n")
	b.WriteString("  classDef accounted fill:#e6f4ea,stroke:#137333,color:#0b4a22;\n")
	b.WriteString("  classDef nseam fill:#fff8f0,stroke:#e02424,stroke-dasharray:4 3,color:#771d1d;\n")
	b.WriteString("  classDef cseam fill:#fffaf0,stroke:#d97706,stroke-dasharray:4 3,color:#7c4a03;\n")
	b.WriteString("  classDef effect fill:#eef2ff,stroke:#3b5bdb,color:#1e3a8a;\n")

	if len(r.NewBlind)+len(r.Carried)+len(r.Accounted) == 0 {
		b.WriteString("  none[\"No structural change to triage\"]\n")
		return b.String()
	}

	effID := map[string]string{}
	var effOrder []string
	effFor := func(label string) string {
		if id, ok := effID[label]; ok {
			return id
		}
		id := fmt.Sprintf("e%d", len(effOrder))
		effID[label] = id
		effOrder = append(effOrder, label)
		return id
	}
	type mmEdge struct{ from, to, style string }
	var edges []mmEdge

	// A blind zone subgraph: each change, its seam node (the blind spots that put it in
	// this zone), and dashed (floor) effect edges.
	blindZone := func(title, idPrefix, nodeClass, seamClass string, fns []ChangedFn, seamsOf func(ChangedFn) []graph.BlindSpot) {
		fmt.Fprintf(&b, "  subgraph %s[\"%s\"]\n", strings.ToUpper(idPrefix), mmLabel(title))
		for i, cf := range fns {
			id := fmt.Sprintf("%s%d", idPrefix, i)
			fmt.Fprintf(&b, "    %s[\"%s\"]:::%s\n", id, mmLabel(nodeLabel(cf)), nodeClass)
			if kinds := distinctKinds(seamsOf(cf)); len(kinds) > 0 {
				sid := id + "s"
				fmt.Fprintf(&b, "    %s{{\"%s\"}}:::%s\n", sid, mmLabel("⚠ "+strings.Join(kinds, ", ")), seamClass)
				edges = append(edges, mmEdge{id, sid, "-.->"})
			}
			for _, eff := range cf.Effects {
				edges = append(edges, mmEdge{id, effFor(eff), "-.->"})
			}
		}
		b.WriteString("  end\n")
	}

	blindZone(fmt.Sprintf("⚠️ New blind — %d (focus)", len(r.NewBlind)), "n", "newblind", "nseam",
		r.NewBlind, func(cf ChangedFn) []graph.BlindSpot { return cf.NewSeams })
	blindZone(fmt.Sprintf("🟡 Carried blind — %d (not new)", len(r.Carried)), "c", "carried", "cseam",
		r.Carried, func(cf ChangedFn) []graph.BlindSpot { return cf.CarriedSeams })

	// Accounted subgraph: solid (complete) effect edges, no seam node.
	fmt.Fprintf(&b, "  subgraph ACCOUNTED[\"%s\"]\n", mmLabel(fmt.Sprintf("✅ Accounted — %d (complete evidence, not approval)", len(r.Accounted))))
	for i, cf := range r.Accounted {
		id := fmt.Sprintf("a%d", i)
		fmt.Fprintf(&b, "    %s[\"%s\"]:::accounted\n", id, mmLabel(nodeLabel(cf)))
		for _, eff := range cf.Effects {
			edges = append(edges, mmEdge{id, effFor(eff), "-->"})
		}
	}
	b.WriteString("  end\n")

	for i, label := range effOrder {
		fmt.Fprintf(&b, "  e%d[[\"%s\"]]:::effect\n", i, mmLabel(label))
	}
	for _, e := range edges {
		fmt.Fprintf(&b, "  %s %s %s\n", e.from, e.style, e.to)
	}
	return b.String()
}

// nodeLabel is the compact function label for the diagram: short name, tier badge, and a
// ✍ marker on a state-mutating change so the eye finds it.
func nodeLabel(cf ChangedFn) string {
	s := fitness.ShortName(cf.FQN)
	if cf.Tier > 0 {
		s += fmt.Sprintf(" [t%d]", cf.Tier)
	}
	if reachesMutating(cf.Effects) {
		s += " ✍"
	}
	return s
}

// distinctKinds is the sorted, deduped set of blind-spot kinds — the at-a-glance "why
// can't the tool see here" the diagram colors.
func distinctKinds(bs []graph.BlindSpot) []string {
	seen := map[string]bool{}
	for _, b := range bs {
		seen[b.Kind] = true
	}
	return setutil.SortedKeys(seen)
}

// mmLabel makes a string safe inside a Mermaid quoted label: collapse the quote that
// would close it, and entity-escape the angle brackets a <dynamic> effect carries.
func mmLabel(s string) string {
	s = strings.ReplaceAll(s, "\"", "'")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// tierTag is the salience badge beside a changed function (omitted for the unset tier).
func tierTag(t int) string {
	if t <= 0 {
		return ""
	}
	return fmt.Sprintf("  [tier %d]", t)
}

// tierLookup maps each branch function to its salience tier.
func tierLookup(g *graph.Graph) map[string]int {
	m := make(map[string]int, len(g.Nodes))
	for _, n := range g.Nodes {
		m[n.FQN] = n.Tier
	}
	return m
}

// nodeSet is the FQN membership set of a graph's nodes.
func nodeSet(g *graph.Graph) map[string]bool {
	m := make(map[string]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		m[n.FQN] = true
	}
	return m
}

// trimmedEffects strips the internal "boundary:" prefix from each effect label for
// display, keeping them sorted.
func trimmedEffects(effects []string) []string {
	out := make([]string, 0, len(effects))
	for _, e := range effects {
		out = append(out, strings.TrimPrefix(e, "boundary:"))
	}
	sort.Strings(out)
	return out
}
