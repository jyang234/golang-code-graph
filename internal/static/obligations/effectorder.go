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
	effectInstrs := map[ssa.Instruction]bool{}
	for _, e := range effects {
		effectInstrs[e.Site] = true
	}
	for _, b := range fn.Blocks {
		for _, in := range b.Instrs {
			c, ok := in.(ssa.CallInstruction)
			if !ok || effectInstrs[in] {
				continue // an effect is not its own fault site
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
	for _, e := range effects {
		for _, c := range fallible {
			can, always := precedes(e.Site, c.instr)
			if !can {
				continue
			}
			out = append(out, EffectOrder{
				Fn:         fn.RelString(nil),
				Effect:     e.Label,
				EffectSite: site(fn, e.Site.Common().Pos(), baseDir, 0),
				Callee:     c.fqn,
				CalleeSite: site(fn, c.instr.Common().Pos(), baseDir, 0),
				Always:     always,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.EffectSite != b.EffectSite {
			return a.EffectSite < b.EffectSite
		}
		if a.CalleeSite != b.CalleeSite {
			return a.CalleeSite < b.CalleeSite
		}
		return a.Effect < b.Effect
	})
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
