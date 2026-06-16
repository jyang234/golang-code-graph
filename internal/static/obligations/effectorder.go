package obligations

import (
	"sort"

	"golang.org/x/tools/go/ssa"
)

// EffectSite is one committed external effect a function makes — a bus publish
// or a DB mutation — paired with its call instruction. graphio collects these
// while classifying boundary edges (the one place labels and ssa sites
// coexist) and hands them here for ordering.
type EffectSite struct {
	Label string // the boundary label, e.g. "boundary:bus PUBLISH loan.approved"
	Site  ssa.CallInstruction
	Via   string // non-empty for a derived site (CX-3): the ALWAYS-effect callee that commits it
}

// EffectOrder is one (committed effect, fallible call) order fact: the effect
// CAN execute before the named call on some path — so if that call faults, the
// effect may already be committed (the partial-effect / inconsistent-state
// case an incident responder needs: "approved-but-uncharged loans"). Always
// strengthens it: the effect dominates the call, so on ANY path reaching the
// fault the effect has happened.
//
// Identity is (Fn, EffectSite, CalleeSite); labels and FQNs are matching keys
// for triage, sites disambiguate repeated calls.
type EffectOrder struct {
	Fn         string `json:"fn"`
	Effect     string `json:"effect"`
	EffectSite string `json:"effect_site"`
	Callee     string `json:"callee"`
	CalleeSite string `json:"callee_site"`
	Always     bool   `json:"always,omitempty"`
	// Via names the ALWAYS-effect callee for a derived site (CX-3): the
	// effect is committed inside that call, proven on its every path.
	// Presentation/provenance only — identity stays (Fn, EffectSite,
	// CalleeSite).
	Via string `json:"via,omitempty"`
}

// OrderFacts computes the intra-CFG partial order between fn's committed
// effects and its fallible call sites (callees whose last result implements
// error — the sites a fault can surface at). A row is emitted iff the effect
// can precede the call on some path; Always marks domination. Functions
// lacking either side produce nothing, which keeps the section small: the
// facts exist only where the partial-effect question can arise.
func OrderFacts(fn *ssa.Function, effects []EffectSite, baseDir string) []EffectOrder {
	if len(effects) == 0 || len(fn.Blocks) == 0 {
		return nil
	}

	type callSite struct {
		instr ssa.CallInstruction
		fqn   string
	}
	var fallible []callSite
	directInstrs := map[ssa.Instruction]bool{}
	for _, e := range effects {
		// Only DIRECT effect sites leave the fault-site list. A derived
		// site's carrier call (CX-3) is still a fault site for OTHER effects:
		// excluding it deleted true pre-existing facts (the loansvc
		// regression — the call to an ALWAYS-publish helper is exactly where
		// a fault surfaces after the direct publishes above it). Its pairing
		// with itself is skipped below instead.
		if e.Via == "" {
			directInstrs[e.Site] = true
		}
	}
	for _, b := range fn.Blocks {
		for _, in := range b.Instrs {
			c, ok := in.(ssa.CallInstruction)
			if !ok || directInstrs[in] {
				continue // a direct effect is not its own fault site
			}
			sig := c.Common().Signature()
			if sig == nil || sig.Results().Len() == 0 {
				continue
			}
			if !isErrorType(sig.Results().At(sig.Results().Len() - 1).Type()) {
				continue
			}
			fallible = append(fallible, callSite{c, calleeFQN(c)})
		}
	}
	if len(fallible) == 0 {
		return nil
	}

	var out []EffectOrder
	for ei, e := range effects {
		for ci, c := range fallible {
			if e.Site == c.instr {
				continue // a derived effect is not its own fault site either
			}
			can, always := precedes(e.Site, c.instr)
			if !can {
				continue
			}
			// Pass the deterministic loop index as the site ordinal (as
			// checkRelease/checkPrecede do): site() uses it only in the
			// invalid-position fallback, where a hard-coded 0 would render two
			// distinct effect/callee sites to the same "#0" string and let
			// dedupeFacts collapse genuinely-distinct facts.
			out = append(out, EffectOrder{
				Fn:         fn.RelString(nil),
				Effect:     e.Label,
				EffectSite: site(fn, e.Site.Common().Pos(), baseDir, ei),
				Callee:     c.fqn,
				CalleeSite: site(fn, c.instr.Common().Pos(), baseDir, ci),
				Always:     always,
				Via:        e.Via,
			})
		}
	}
	// A TOTAL order over every field — not just the (EffectSite, CalleeSite,
	// Effect) display key. sort.Slice is not stable, so any facts that tied on a
	// partial key could land in a run-dependent order; ordering on all fields
	// makes the emitted section a pure function of the graph AND guarantees
	// byte-identical duplicates are adjacent, which is what dedupeFacts collapses.
	sort.Slice(out, func(i, j int) bool { return less(out[i], out[j]) })
	return dedupeFacts(out)
}

// less is the total order over EffectOrder used to make emission deterministic.
func less(a, b EffectOrder) bool {
	switch {
	case a.EffectSite != b.EffectSite:
		return a.EffectSite < b.EffectSite
	case a.CalleeSite != b.CalleeSite:
		return a.CalleeSite < b.CalleeSite
	case a.Effect != b.Effect:
		return a.Effect < b.Effect
	case a.Callee != b.Callee:
		return a.Callee < b.Callee
	case a.Fn != b.Fn:
		return a.Fn < b.Fn
	case a.Always != b.Always:
		return !a.Always // false before true, a stable convention
	default:
		return a.Via < b.Via
	}
}

// dedupeFacts drops exact-duplicate order facts. The effect/fallible double loop
// pairs every collected effect site with every fault site, so a function that
// records the same effect site more than once (a derived site that coincides
// with a direct one, repeated collection) would otherwise emit byte-identical
// rows. Identity here is the whole fact — distinct sites are preserved; only
// genuine duplicates collapse, keeping the graph's effect_order section honest
// about how many distinct facts it carries.
func dedupeFacts(in []EffectOrder) []EffectOrder {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, f := range in[1:] {
		if f != out[len(out)-1] {
			out = append(out, f)
		}
	}
	return out
}

// precedes reports whether instruction e can execute before c on some path
// (same block and earlier, or c's block reachable from e's block), and whether
// it ALWAYS does (same-block straight line, or e's block strictly dominates
// c's).
func precedes(e, c ssa.Instruction) (can, always bool) {
	eb, cb := e.Block(), c.Block()
	if eb == cb {
		for _, in := range eb.Instrs {
			if in == e {
				return true, true // straight line: e runs before c whenever c runs
			}
			if in == c {
				return false, false
			}
		}
		return false, false
	}
	if eb.Dominates(cb) {
		return true, true
	}
	// Forward reachability eb → cb.
	seen := map[*ssa.BasicBlock]bool{eb: true}
	queue := []*ssa.BasicBlock{eb}
	for len(queue) > 0 {
		b := queue[0]
		queue = queue[1:]
		for _, next := range b.Succs {
			if next == cb {
				return true, false
			}
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false, false
}

// calleeFQN names a fallible call's target for triage matching: the static
// callee's FQN, or the interface method's full name for invoke-mode calls.
func calleeFQN(c ssa.CallInstruction) string {
	common := c.Common()
	if common.IsInvoke() {
		return common.Method.FullName()
	}
	if callee := common.StaticCallee(); callee != nil {
		return callee.RelString(nil)
	}
	return "<dynamic>"
}
