// Summaries: the interprocedural summary engine (correctness-expansion plan,
// CX-0). Three-valued, per (target set, function) answers composed bottom-up
// and top-down over the call graph, so the obligation checks can consult a
// callee or a caller without ever guessing:
//
//   - Discharges (bottom-up): ALWAYS — every path from entry to every exit
//     passes a call matching the targets (the claim inlining would have
//     produced; value-blind like the intraprocedural walk); NEVER — no
//     matching call is reachable in the function's transitive cone and the
//     cone touches no frontier (sound because the call graph over-
//     approximates); UNKNOWN — everything else: matching calls on some paths
//     only, recursion (any cyclic SCC member), recover, a frontier in the
//     cone, or a body the unit cannot see.
//   - EntryDominated (top-down, D-CX7): has a plain call to the require
//     target already executed on every entry into this function? ALWAYS when
//     every call edge in is dominated in its caller by a require site (a
//     direct match or a derived-A: a call whose callees all Discharge ALWAYS)
//     or arrives from a caller that is itself entry-dominated; NEVER when a
//     provably require-less entry chain exists (a graph source reaches the
//     function undominated); UNKNOWN otherwise — including any function whose
//     address is taken, because an invisible dynamic caller may exist.
//
// Trust monotonicity (D-CX2) is the consumers' contract, enabled here by
// construction: ALWAYS and the ED poles are only ever proofs, NEVER only ever
// follows from over-approximated reachability, and everything unprovable is
// UNKNOWN — never silently treated as either pole.
//
// Determinism: the universe is sorted at construction, SCCs are computed over
// the sorted adjacency (members of any cyclic SCC are UNKNOWN for ALWAYS — no
// fixed-point iteration, D-CX1), per-edge statuses are folded order-
// independently, and every answer is memoized pure output of (unit, targets).
package obligations

import (
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// Summary is the three-valued answer to one interprocedural question.
type Summary uint8

const (
	// SummaryUnknown: the question is not provable either way here; the
	// consumer must abstain legibly, never default.
	SummaryUnknown Summary = iota
	// SummaryAlways: proven on every modeled path / entry.
	SummaryAlways
	// SummaryNever: proven unreachable / a provably target-less entry exists.
	SummaryNever
)

func (s Summary) String() string {
	switch s {
	case SummaryAlways:
		return "ALWAYS"
	case SummaryNever:
		return "NEVER"
	default:
		return "UNKNOWN"
	}
}

// Unit is the slice of the call graph the summary engine reads. Fns is the
// analyzed universe; Callees must enumerate every possible in-universe callee
// of a site per the call graph's over-approximation (RTA/CHA candidates for
// invoke-mode calls, the static callee otherwise). A site Callees cannot
// enumerate — a dynamic function value, an unresolved invoke — is a frontier:
// it blocks NEVER and earns no ALWAYS credit. Calls resolving outside Fns are
// frontiers too; the unit's edge is the proof's edge.
type Unit struct {
	Fns     []*ssa.Function
	Callees func(site ssa.CallInstruction) []*ssa.Function
}

// Summaries memoizes the engine's answers over one Unit. Construct with
// NewSummaries; methods are pure functions of (unit, arguments).
type Summaries struct {
	fns     []*ssa.Function // sorted universe
	member  map[*ssa.Function]bool
	callees func(site ssa.CallInstruction) []*ssa.Function

	sccOf  map[*ssa.Function]int
	cyclic map[int]bool

	taken   map[*ssa.Function]bool        // lazily built: address-taken functions
	inEdges map[*ssa.Function][]entryEdge // lazily built: resolver-visible call edges in
	targets map[string][]ref              // interned target sets by key
	disch   map[summaryKey]Summary        // Discharges memo
	edom    map[summaryKey]Summary        // EntryDominated memo
	aSites  map[summaryKey][]domSite      // per (require, caller): A sites for dominance
}

type summaryKey struct {
	fn  *ssa.Function
	key string
}

type entryEdge struct {
	caller *ssa.Function
	site   ssa.CallInstruction
}

type domSite struct {
	block *ssa.BasicBlock
	index int
}

// NewSummaries builds the engine over one unit: sorts the universe (input
// order must not matter) and computes the SCC condensation eagerly so every
// later answer is order-independent.
func NewSummaries(u *Unit) *Summaries {
	fns := append([]*ssa.Function(nil), u.Fns...)
	sort.SliceStable(fns, func(i, j int) bool {
		a, b := fns[i], fns[j]
		if as, bs := a.String(), b.String(); as != bs {
			return as < bs
		}
		return a.Pos() < b.Pos() // generic instantiations can share a name
	})
	s := &Summaries{
		fns:     fns,
		member:  make(map[*ssa.Function]bool, len(fns)),
		callees: u.Callees,
		targets: map[string][]ref{},
		disch:   map[summaryKey]Summary{},
		edom:    map[summaryKey]Summary{},
		aSites:  map[summaryKey][]domSite{},
	}
	for _, fn := range fns {
		s.member[fn] = true
	}
	s.computeSCC()
	return s
}

// Discharges answers the bottom-up question: does fn, on every path from
// entry to every exit, call one of targets (ALWAYS); provably never reach one
// (NEVER); or neither (UNKNOWN)? Targets use the rule "import/path#Symbol"
// form and must be well-formed (config validation owns that).
func (s *Summaries) Discharges(fn *ssa.Function, targets []string) Summary {
	return s.dischargeKey(fn, s.intern(targets))
}

// EntryDominated answers the top-down question (D-CX7): has a plain call to
// require executed before every entry into fn?
func (s *Summaries) EntryDominated(fn *ssa.Function, require string) Summary {
	key := s.intern([]string{require})
	if !s.member[fn] {
		return SummaryUnknown
	}
	k := summaryKey{fn, key}
	if v, ok := s.edom[k]; ok {
		return v
	}
	res := s.entryDominated(fn, key)
	s.edom[k] = res
	return res
}

func (s *Summaries) intern(targets []string) string {
	key := strings.Join(targets, "\x00")
	if _, ok := s.targets[key]; !ok {
		s.targets[key] = parseRefs(targets)
	}
	return key
}

// ---- bottom-up: Discharges ----------------------------------------------------

func (s *Summaries) dischargeKey(fn *ssa.Function, key string) Summary {
	if !s.member[fn] {
		return SummaryUnknown
	}
	k := summaryKey{fn, key}
	if v, ok := s.disch[k]; ok {
		return v
	}
	var res Summary
	switch {
	case s.never(fn, key):
		// Reachability needs no CFG induction, so it survives recursion and
		// recover: a closed cone with no target cannot discharge, period.
		res = SummaryNever
	case s.cyclic[s.sccOf[fn]]:
		res = SummaryUnknown // D-CX1: cyclic SCC members abstain, no fixed point
	case len(fn.Blocks) == 0 || usesRecover(fn):
		res = SummaryUnknown
	case s.alwaysWalk(fn, key):
		res = SummaryAlways
	default:
		res = SummaryUnknown
	}
	s.disch[k] = res
	return res
}

// never reports whether no target-matching call is reachable in fn's
// transitive cone, with the cone fully visible (no frontier, no bodyless
// member). Sound under the call graph's over-approximation.
func (s *Summaries) never(fn *ssa.Function, key string) bool {
	refs := s.targets[key]
	seen := map[*ssa.Function]bool{fn: true}
	queue := []*ssa.Function{fn}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		if len(f.Blocks) == 0 {
			return false // body invisible: the cone is open
		}
		for _, b := range f.Blocks {
			for _, in := range b.Instrs {
				site, ok := in.(ssa.CallInstruction)
				if !ok {
					continue
				}
				if anyRef(refs, site) {
					return false // a target is reachable (go/defer included)
				}
				cands, frontier := s.resolve(site)
				if frontier {
					return false
				}
				for _, c := range cands {
					if !seen[c] {
						seen[c] = true
						queue = append(queue, c)
					}
				}
			}
		}
	}
	return true
}

// alwaysWalk mirrors leakPath from the function's entry: is any exit (return,
// explicit panic) reachable without coverage? Coverage is a direct target
// call, a defer covering later exits (deferReleases' rules, plus the D-CX7
// named-helper lift), or a call/defer whose resolved callees all Discharge
// ALWAYS. Goroutine spawns never credit (concurrent discharge is out of
// scope); implicit runtime panics are ignored, as the intraprocedural walk
// ignores them.
func (s *Summaries) alwaysWalk(fn *ssa.Function, key string) bool {
	refs := s.targets[key]
	visited := map[*ssa.BasicBlock]bool{}
	var walk func(b *ssa.BasicBlock, from int) bool // true: uncovered exit reachable
	walk = func(b *ssa.BasicBlock, from int) bool {
		for i := from; i < len(b.Instrs); i++ {
			switch in := b.Instrs[i].(type) {
			case *ssa.Call:
				if anyRef(refs, in) || s.creditCall(in, key) {
					return false // covered: this path discharges
				}
			case *ssa.Defer:
				if deferReleases(in, refs) || s.creditCall(in, key) {
					return false // registered discharge covers every later exit
				}
			case *ssa.Return:
				return true
			case *ssa.Panic:
				return true
			}
		}
		for _, next := range b.Succs {
			if visited[next] {
				continue
			}
			visited[next] = true
			if walk(next, 0) {
				return true
			}
		}
		return false
	}
	return !walk(fn.Blocks[0], 0)
}

// creditCall reports whether a call site provably discharges via its callees:
// the site has no frontier and every resolved callee Discharges ALWAYS.
func (s *Summaries) creditCall(site ssa.CallInstruction, key string) bool {
	cands, frontier := s.resolve(site)
	if frontier || len(cands) == 0 {
		return false
	}
	for _, c := range cands {
		if s.dischargeKey(c, key) != SummaryAlways {
			return false
		}
	}
	return true
}

// resolve classifies one call site: its in-universe callees, and whether the
// site can dispatch somewhere the unit cannot see (a frontier). Builtins are
// neither. A resolved member whose body is invisible is a frontier too.
func (s *Summaries) resolve(site ssa.CallInstruction) (cands []*ssa.Function, frontier bool) {
	common := site.Common()
	if _, ok := common.Value.(*ssa.Builtin); ok {
		return nil, false
	}
	raw := s.callees(site)
	if len(raw) == 0 {
		if sc := common.StaticCallee(); sc != nil {
			raw = []*ssa.Function{sc}
		} else {
			return nil, true // dynamic value or unresolved invoke
		}
	}
	for _, c := range raw {
		if !s.member[c] || len(c.Blocks) == 0 {
			frontier = true
			continue
		}
		cands = append(cands, c)
	}
	return cands, frontier
}

// ---- top-down: EntryDominated --------------------------------------------------

func (s *Summaries) entryDominated(fn *ssa.Function, key string) Summary {
	if s.addressTaken(fn) {
		return SummaryUnknown // an invisible dynamic caller may exist
	}
	edges := s.entries(fn)
	if len(edges) == 0 {
		return SummaryNever // a graph source is entered with nothing behind it
	}
	allDominated, provenOpen := true, false
	for _, e := range edges {
		if s.dominatedEntry(e, key) {
			continue
		}
		var st Summary
		if s.sccOf[e.caller] == s.sccOf[fn] {
			st = SummaryUnknown // recursion: abstain, no fixed point
		} else {
			st = s.EntryDominated(e.caller, s.requireOf(key))
		}
		switch st {
		case SummaryAlways:
			// dominated at every entry to the caller — so before this site too
		case SummaryNever:
			provenOpen = true
			allDominated = false
		default:
			allDominated = false
		}
	}
	switch {
	case allDominated:
		return SummaryAlways
	case provenOpen:
		return SummaryNever
	default:
		return SummaryUnknown
	}
}

// requireOf recovers the single-target form for the recursive consult; an
// EntryDominated key is always a one-element set.
func (s *Summaries) requireOf(key string) string { return key }

// dominatedEntry reports whether the edge's call site is dominated in its
// caller by a require site: a plain call matching the target (a deferred
// require runs at exit, after the entry it must precede — checkPrecede's
// rule), or a derived-A — a plain call whose callees all Discharge ALWAYS.
func (s *Summaries) dominatedEntry(e entryEdge, key string) bool {
	sb := e.site.Block()
	if sb == nil {
		return false
	}
	si := -1
	for i, in := range sb.Instrs {
		if in == e.site {
			si = i
			break
		}
	}
	for _, a := range s.requireSites(e.caller, key) {
		if (a.block == sb && a.index < si) || (a.block != sb && a.block.Dominates(sb)) {
			return true
		}
	}
	return false
}

func (s *Summaries) requireSites(caller *ssa.Function, key string) []domSite {
	k := summaryKey{caller, key}
	if v, ok := s.aSites[k]; ok {
		return v
	}
	refs := s.targets[key]
	sites := []domSite{}
	for _, b := range caller.Blocks {
		for i, in := range b.Instrs {
			call, ok := in.(*ssa.Call) // plain calls only
			if !ok {
				continue
			}
			if anyRef(refs, call) || s.creditCall(call, key) {
				sites = append(sites, domSite{b, i})
			}
		}
	}
	s.aSites[k] = sites
	return sites
}

// entries returns every resolver-visible call edge into fn, built once for
// the whole unit in universe order (deterministic). Frontier sites add no
// edges; the addressTaken guard is what keeps that sound — a dynamic value
// can only dispatch to a function whose address was taken.
func (s *Summaries) entries(fn *ssa.Function) []entryEdge {
	if s.inEdges == nil {
		s.inEdges = map[*ssa.Function][]entryEdge{}
		for _, caller := range s.fns {
			for _, b := range caller.Blocks {
				for _, in := range b.Instrs {
					site, ok := in.(ssa.CallInstruction)
					if !ok {
						continue
					}
					cands, _ := s.resolve(site)
					for _, c := range cands {
						s.inEdges[c] = append(s.inEdges[c], entryEdge{caller, site})
					}
				}
			}
		}
	}
	return s.inEdges[fn]
}

// addressTaken reports whether fn is ever used as a value — stored, captured,
// converted, or passed as an argument — anywhere in the universe. Being the
// direct callee of a call is not a use-as-value.
func (s *Summaries) addressTaken(fn *ssa.Function) bool {
	if s.taken == nil {
		s.taken = map[*ssa.Function]bool{}
		var rands []*ssa.Value
		for _, f := range s.fns {
			for _, b := range f.Blocks {
				for _, in := range b.Instrs {
					if ci, ok := in.(ssa.CallInstruction); ok {
						for _, a := range ci.Common().Args {
							if g, ok := a.(*ssa.Function); ok {
								s.taken[g] = true
							}
						}
						continue
					}
					rands = in.Operands(rands[:0])
					for _, r := range rands {
						if r == nil || *r == nil {
							continue
						}
						if g, ok := (*r).(*ssa.Function); ok {
							s.taken[g] = true
						}
					}
				}
			}
		}
	}
	return s.taken[fn]
}

// ---- SCC condensation ----------------------------------------------------------

// computeSCC runs Kosaraju over the resolver-visible adjacency, iteratively
// (real call chains are deep) and in universe order, so component identity is
// a pure function of the unit. cyclic marks components that can re-enter
// themselves (size > 1, or a self edge).
func (s *Summaries) computeSCC() {
	succ := make(map[*ssa.Function][]*ssa.Function, len(s.fns))
	pred := make(map[*ssa.Function][]*ssa.Function, len(s.fns))
	for _, fn := range s.fns {
		seen := map[*ssa.Function]bool{}
		for _, b := range fn.Blocks {
			for _, in := range b.Instrs {
				site, ok := in.(ssa.CallInstruction)
				if !ok {
					continue
				}
				cands, _ := s.resolve(site)
				for _, c := range cands {
					if !seen[c] {
						seen[c] = true
						succ[fn] = append(succ[fn], c)
						pred[c] = append(pred[c], fn)
					}
				}
			}
		}
	}

	// Pass 1: finish order.
	type frame struct {
		fn *ssa.Function
		i  int
	}
	visited := make(map[*ssa.Function]bool, len(s.fns))
	order := make([]*ssa.Function, 0, len(s.fns))
	for _, root := range s.fns {
		if visited[root] {
			continue
		}
		visited[root] = true
		stack := []frame{{root, 0}}
		for len(stack) > 0 {
			f := &stack[len(stack)-1]
			if f.i < len(succ[f.fn]) {
				next := succ[f.fn][f.i]
				f.i++
				if !visited[next] {
					visited[next] = true
					stack = append(stack, frame{next, 0})
				}
				continue
			}
			order = append(order, f.fn)
			stack = stack[:len(stack)-1]
		}
	}

	// Pass 2: components over reversed edges, in reverse finish order.
	s.sccOf = make(map[*ssa.Function]int, len(s.fns))
	s.cyclic = map[int]bool{}
	next := 0
	for i := len(order) - 1; i >= 0; i-- {
		root := order[i]
		if _, done := s.sccOf[root]; done {
			continue
		}
		id := next
		next++
		s.sccOf[root] = id
		size := 1
		stack := []*ssa.Function{root}
		for len(stack) > 0 {
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, p := range pred[f] {
				if _, done := s.sccOf[p]; !done {
					s.sccOf[p] = id
					size++
					stack = append(stack, p)
				}
			}
		}
		s.cyclic[id] = size > 1
	}
	for fn, out := range succ {
		for _, c := range out {
			if c == fn {
				s.cyclic[s.sccOf[fn]] = true
			}
		}
	}
}
