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
//     every call edge in is require-covered in its caller (every caller-entry
//     →site path passes a require: a direct match or a derived-A — a call
//     whose callees all Discharge ALWAYS) or arrives from a caller that is
//     itself entry-dominated; UNKNOWN otherwise — including any function
//     whose address is taken, any method a frontier invoke could dispatch,
//     and any graph source, because unseen or out-of-unit entries may exist.
//     EntryDominated deliberately has no NEVER pole (adversarial review F3):
//     "provably require-less" would overclaim, so the consumer keeps its own
//     intraprocedural verdict instead.
//
// Trust monotonicity (D-CX2) is the consumers' contract, enabled here by
// construction: ALWAYS and entry-domination are only ever proofs, NEVER only
// ever follows from over-approximated reachability, and everything unprovable
// is UNKNOWN — never silently treated as either pole.
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
	"golang.org/x/tools/go/ssa/ssautil"
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

// Unit is the slice of the call graph the summary engine reads. Two adapter
// obligations are load-bearing for soundness (adversarial review F4):
//
//   - Fns must be the COMPLETE built universe — anonymous functions,
//     synthetic wrappers, and package initializers included. Address-taking
//     and call edges in code Fns omits are invisible, and invisible means
//     unsound, not conservative.
//   - Callees must enumerate every possible callee of a site per the call
//     graph's over-approximation (RTA/CHA candidates for invoke-mode calls,
//     the static callee otherwise), INCLUDING callees outside Fns — the
//     engine classifies those as frontiers itself. Returning a pre-filtered
//     set makes a partially-resolvable site look fully resolved: a false
//     proof, not a missed one.
//
// A site Callees cannot enumerate — a dynamic function value, an unresolved
// invoke — is a frontier: it blocks NEVER, earns no ALWAYS credit, and (by
// method name) abstains entry-domination for same-named methods.
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
	sccFns map[int][]*ssa.Function

	taken    map[*ssa.Function]bool                  // lazily built: address-taken functions
	inEdges  map[*ssa.Function][]entryEdge           // lazily built: resolver-visible call edges in
	dynInvok map[string]bool                         // lazily built (with inEdges): method names at unresolved invoke sites
	targets  map[targetKey][]ref                     // interned ref target sets
	siteSets map[targetKey]map[ssa.Instruction]bool  // interned effect-site sets (CX-3)
	disch    map[summaryKey]Summary                  // Discharges memo
	nev      map[summaryKey]bool                     // never memo, SCC-shared
	edom     map[summaryKey]edomResult               // EntryDominated memo (verdict + witness note)
	aInstrs  map[summaryKey]map[ssa.Instruction]bool // per (require, caller): A-site instructions
}

type edomResult struct {
	sum  Summary
	note string
}

// targetKey identifies one interned target set. kind separates rule-ref sets
// from effect-label sets structurally, so the two families can never collide
// by string convention (code-review finding: replaces the former
// "\x00effect:" namespace packing).
type targetKey struct {
	kind byte // 'r': rule refs; 'e': effect label
	name string
}

type summaryKey struct {
	fn  *ssa.Function
	key targetKey
}

type entryEdge struct {
	caller *ssa.Function
	site   ssa.CallInstruction
}

// NewProgramSummaries is the production constructor: the universe is every
// function of the BUILT program — package initializers, anonymous functions,
// and wrappers included — so the F4 completeness precondition holds by
// construction instead of by an adapter's discipline (code-review finding).
// callees supplies the call graph's over-approximation per site; sites it
// has no entry for fall back to their static callee inside the engine, and
// everything else is a frontier. The remaining adapter obligation (never
// pre-filter the candidate set) still rests on the caller.
func NewProgramSummaries(prog *ssa.Program, callees func(site ssa.CallInstruction) []*ssa.Function) *Summaries {
	all := ssautil.AllFunctions(prog)
	fns := make([]*ssa.Function, 0, len(all))
	for fn := range all {
		fns = append(fns, fn)
	}
	return NewSummaries(&Unit{Fns: fns, Callees: callees}) // NewSummaries sorts
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
		fns:      fns,
		member:   make(map[*ssa.Function]bool, len(fns)),
		callees:  u.Callees,
		targets:  map[targetKey][]ref{},
		siteSets: map[targetKey]map[ssa.Instruction]bool{},
		disch:    map[summaryKey]Summary{},
		nev:      map[summaryKey]bool{},
		edom:     map[summaryKey]edomResult{},
		aInstrs:  map[summaryKey]map[ssa.Instruction]bool{},
	}
	for _, fn := range fns {
		s.member[fn] = true
	}
	s.computeSCC()
	return s
}

// inUnit reports universe membership. member looks redundant with sccOf
// presence, but it is NOT derivable from it: resolve() consults membership
// while computeSCC is still BUILDING sccOf (the adjacency pass), and an empty
// membership there marks every candidate a frontier — erasing self-loops from
// the condensation and unleashing unbounded creditCall recursion. The
// separate map is the bootstrap order made explicit; a post-construction
// assertion that the two key sets agree lives in the unit tests.
func (s *Summaries) inUnit(fn *ssa.Function) bool {
	return s.member[fn]
}

// Discharges answers the bottom-up question: does fn, on every path from
// entry to every exit, call one of targets (ALWAYS); provably never reach one
// (NEVER); or neither (UNKNOWN)? Targets use the rule "import/path#Symbol"
// form and must be well-formed (config validation owns that).
func (s *Summaries) Discharges(fn *ssa.Function, targets []string) Summary {
	return s.dischargeKey(fn, s.intern(targets))
}

// AlwaysEffect reports whether fn performs the labeled committed effect on
// every path from entry to every exit (CX-3): a call site classified under
// the label (sites — the label's instruction set across the unit), or a
// call/defer to a callee that itself AlwaysEffect. A deferred effect runs
// before the frame exits, so it counts; a spawned one does not. The label
// keys the memo, so repeated queries for one label share all work; sites must
// be the same set on every call for a given label.
func (s *Summaries) AlwaysEffect(fn *ssa.Function, label string, sites map[ssa.Instruction]bool) bool {
	key := targetKey{'e', label}
	if prev, ok := s.siteSets[key]; ok {
		// Memoized answers were computed against the first binding; a caller
		// re-binding the label to a DIFFERENT set would silently get stale
		// verdicts. That is an API-contract bug, so it fails loudly.
		if !sameSites(prev, sites) {
			panic("obligations: AlwaysEffect re-bound label " + label + " with a different site set")
		}
	} else {
		s.siteSets[key] = sites
	}
	return s.dischargeKey(fn, key) == SummaryAlways
}

func sameSites(a, b map[ssa.Instruction]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// EntryDominated answers the top-down question (D-CX7): has a plain call to
// require executed before every entry into fn?
func (s *Summaries) EntryDominated(fn *ssa.Function, require string) Summary {
	res, _ := s.EntryDominatedNote(fn, require)
	return res
}

// EntryDominatedNote is EntryDominated plus a deterministic witness note for
// the finding's detail: the first satisfied entry for ALWAYS, the abstention
// reason otherwise. Presentation only — never part of a finding's identity
// (D-OB6).
func (s *Summaries) EntryDominatedNote(fn *ssa.Function, require string) (Summary, string) {
	key := s.intern([]string{require})
	if !s.inUnit(fn) {
		return SummaryUnknown, "the function is outside the analyzed unit"
	}
	return s.entryDominatedMemo(fn, key)
}

func (s *Summaries) entryDominatedMemo(fn *ssa.Function, key targetKey) (Summary, string) {
	k := summaryKey{fn, key}
	if v, ok := s.edom[k]; ok {
		return v.sum, v.note
	}
	sum, note := s.entryDominated(fn, key)
	s.edom[k] = edomResult{sum, note}
	return sum, note
}

// DerivedRequire reports whether a plain call provably executes the require on
// every path before returning — a derived A site (D-CX7). Unconditional for
// every must-precede rule: it widens the recognition of A sites within the
// same function, not the rule's scope (D-CX9).
func (s *Summaries) DerivedRequire(call *ssa.Call, require string) bool {
	return s.creditCall(call, s.intern([]string{require}))
}

func (s *Summaries) intern(targets []string) targetKey {
	key := targetKey{'r', strings.Join(targets, "\x00")}
	if _, ok := s.targets[key]; !ok {
		s.targets[key] = parseRefs(targets)
	}
	return key
}

// ---- bottom-up: Discharges ----------------------------------------------------

func (s *Summaries) dischargeKey(fn *ssa.Function, key targetKey) Summary {
	if !s.inUnit(fn) {
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
// member). Sound under the call graph's over-approximation. Computed per SCC
// over the condensation and memoized — cones overlap heavily, and a per-query
// BFS would be quadratic on a whole-program universe.
func (s *Summaries) never(fn *ssa.Function, key targetKey) bool {
	k := summaryKey{fn, key}
	if v, ok := s.nev[k]; ok {
		return v
	}
	id := s.sccOf[fn]
	members := s.sccFns[id]

	// The whole component shares one answer: every member reaches every other.
	clean := true
	var ext []*ssa.Function // out-of-component successors, deduped
	seen := map[*ssa.Function]bool{}
	for _, m := range members {
		if len(m.Blocks) == 0 {
			clean = false // body invisible: the cone is open
			break
		}
		for _, b := range m.Blocks {
			for _, in := range b.Instrs {
				site, ok := in.(ssa.CallInstruction)
				if !ok {
					continue
				}
				if s.hits(key, site) {
					clean = false // a target is reachable (go/defer included)
					break
				}
				cands, frontier := s.resolve(site)
				if frontier {
					clean = false
					break
				}
				for _, c := range cands {
					if s.sccOf[c] != id && !seen[c] {
						seen[c] = true
						ext = append(ext, c)
					}
				}
			}
			if !clean {
				break
			}
		}
		if !clean {
			break
		}
	}
	// The recursion below steps strictly down the condensation DAG (an
	// external successor's cone cannot re-enter this component), so it
	// terminates and never observes a half-computed answer.
	res := clean
	if res {
		for _, c := range ext {
			if !s.never(c, key) {
				res = false
				break
			}
		}
	}
	for _, m := range members {
		s.nev[summaryKey{m, key}] = res
	}
	return res
}

// alwaysWalk mirrors leakPath from the function's entry: is any exit (return,
// explicit panic) reachable without coverage? Coverage is a direct target
// call, a deferred target, or a call/defer whose resolved callees all
// Discharge ALWAYS — which is how a deferred named helper OR anonymous
// closure earns credit: by its own all-paths summary. deferReleases'
// any-instruction closure scan is deliberately NOT reused here (adversarial
// review F1): it accepts a release under an `if` inside the closure, which is
// fine for the intraprocedural verdict's documented idiom but would mint a
// portable ALWAYS the closure cannot back. Goroutine spawns never credit
// (concurrent discharge is out of scope); implicit runtime panics are
// ignored, as the intraprocedural walk ignores them.
func (s *Summaries) alwaysWalk(fn *ssa.Function, key targetKey) bool {
	visited := map[*ssa.BasicBlock]bool{}
	var walk func(b *ssa.BasicBlock, from int) bool // true: uncovered exit reachable
	walk = func(b *ssa.BasicBlock, from int) bool {
		for i := from; i < len(b.Instrs); i++ {
			switch in := b.Instrs[i].(type) {
			case *ssa.Call:
				if s.hits(key, in) || s.creditCall(in, key) {
					return false // covered: this path discharges
				}
			case *ssa.Defer:
				if s.hits(key, in) || s.creditCall(in, key) {
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
	// Seed the entry as visited: every block is entered at index 0, so a
	// back-edge to the entry only re-scans already-explored instructions.
	visited[fn.Blocks[0]] = true
	return !walk(fn.Blocks[0], 0)
}

// creditCall reports whether a call site provably discharges via its callees:
// the site has no frontier and every resolved callee Discharges ALWAYS.
func (s *Summaries) creditCall(site ssa.CallInstruction, key targetKey) bool {
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

// hits reports whether one call instruction matches the key's targets — a
// ref match (rule anchors) or membership in the key's instruction set
// (classified effect sites, CX-3).
func (s *Summaries) hits(key targetKey, in ssa.CallInstruction) bool {
	if set := s.siteSets[key]; set != nil && set[in] {
		return true
	}
	return anyRef(s.targets[key], in)
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
		if !s.inUnit(c) || len(c.Blocks) == 0 {
			frontier = true
			continue
		}
		cands = append(cands, c)
	}
	return cands, frontier
}

// ---- top-down: EntryDominated --------------------------------------------------

// entryDominated proves at most one pole (adversarial review F3): ALWAYS, or
// a disclosed UNKNOWN. It never claims NEVER — "provably require-less entry"
// would have to reason about package initializers, out-of-unit callers, and
// require-avoiding paths it cannot enumerate; the consumer keeps its own
// intraprocedural VIOLATED instead of borrowing a witness the engine cannot
// back.
func (s *Summaries) entryDominated(fn *ssa.Function, key targetKey) (Summary, string) {
	if s.addressTaken(fn) {
		return SummaryUnknown, "its address is taken — an unseen dynamic caller may exist"
	}
	if fn.Signature.Recv() != nil && s.dynamicInvokeName(fn.Name()) {
		// Adversarial review F2: an interface method is entered by dispatch
		// without its address ever being an SSA operand, so an unresolved
		// invoke of this name anywhere is a possible unseen entry.
		return SummaryUnknown, "an unresolved interface dispatch of " + fn.Name() + " exists — an unseen entry may exist"
	}
	edges := s.entries(fn)
	if len(edges) == 0 {
		return SummaryUnknown, "no callers in the unit — its entries are beyond proof"
	}
	var firstGood, firstUnknown *ssa.Function
	for _, e := range edges {
		if s.entryCovered(e, key) {
			if firstGood == nil {
				firstGood = e.caller
			}
			continue
		}
		st := SummaryUnknown
		if s.sccOf[e.caller] != s.sccOf[fn] { // recursion abstains, no fixed point
			st, _ = s.entryDominatedMemo(e.caller, key)
		}
		if st == SummaryAlways {
			// covered at every entry to the caller — so before this site too
			if firstGood == nil {
				firstGood = e.caller
			}
			continue
		}
		if firstUnknown == nil {
			firstUnknown = e.caller
		}
	}
	if firstUnknown == nil {
		return SummaryAlways, "e.g. entered via " + firstGood.RelString(nil)
	}
	return SummaryUnknown, "entry via " + firstUnknown.RelString(nil) + " is beyond proof"
}

// entryCovered reports whether every path from the caller's entry to the
// edge's call site passes a require site first — a coverage walk, not a
// dominance query (adversarial review F3a): two A sites on the arms of a
// branch cover the join without either dominating it, and coverage is the
// property execution actually has. Conservative everywhere it must be: a
// caller using recover has an untrustworthy CFG, and reaching the site
// uncovered on any modeled path refuses the edge.
func (s *Summaries) entryCovered(e entryEdge, key targetKey) bool {
	caller := e.caller
	if len(caller.Blocks) == 0 || usesRecover(caller) {
		return false
	}
	as := s.requireSites(caller, key)
	visited := map[*ssa.BasicBlock]bool{}
	var walk func(b *ssa.BasicBlock, from int) bool // true: site reachable uncovered
	walk = func(b *ssa.BasicBlock, from int) bool {
		for i := from; i < len(b.Instrs); i++ {
			in := b.Instrs[i]
			if in == e.site {
				return true // reached the entry with no require behind it
			}
			if as[in] {
				return false // a require covers everything past this point
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
	// Seed the entry as visited: every block is entered at index 0, so a
	// back-edge to the entry only re-scans already-explored instructions.
	visited[caller.Blocks[0]] = true
	return !walk(caller.Blocks[0], 0)
}

// requireSites collects the caller's A-site instructions: plain calls
// matching the require (a deferred require runs at exit, after the entry it
// must precede — checkPrecede's rule), and derived-A calls — plain calls
// whose callees all Discharge ALWAYS.
func (s *Summaries) requireSites(caller *ssa.Function, key targetKey) map[ssa.Instruction]bool {
	k := summaryKey{caller, key}
	if v, ok := s.aInstrs[k]; ok {
		return v
	}
	sites := map[ssa.Instruction]bool{}
	for _, b := range caller.Blocks {
		for _, in := range b.Instrs {
			call, ok := in.(*ssa.Call) // plain calls only
			if !ok {
				continue
			}
			if s.hits(key, call) || s.creditCall(call, key) {
				sites[in] = true
			}
		}
	}
	s.aInstrs[k] = sites
	return sites
}

// entries returns every resolver-visible call edge into fn, built once for
// the whole unit in universe order (deterministic). Frontier sites add no
// edges; two guards keep that sound — a dynamic function value can only
// dispatch to a function whose address was taken (addressTaken), and an
// unresolved invoke can only dispatch to a method whose name it carries
// (dynamicInvokeName, adversarial review F2). The same pass collects those
// unresolved invoke names.
func (s *Summaries) entries(fn *ssa.Function) []entryEdge {
	if s.inEdges == nil {
		s.inEdges = map[*ssa.Function][]entryEdge{}
		s.dynInvok = map[string]bool{}
		for _, caller := range s.fns {
			for _, b := range caller.Blocks {
				for _, in := range b.Instrs {
					site, ok := in.(ssa.CallInstruction)
					if !ok {
						continue
					}
					cands, frontier := s.resolve(site)
					if frontier && site.Common().IsInvoke() {
						s.dynInvok[site.Common().Method.Name()] = true
					}
					for _, c := range cands {
						s.inEdges[c] = append(s.inEdges[c], entryEdge{caller, site})
					}
				}
			}
		}
	}
	return s.inEdges[fn]
}

// dynamicInvokeName reports whether any unresolved invoke-mode call site in
// the unit dispatches a method of this name — a possible unseen entry into
// any same-named method.
func (s *Summaries) dynamicInvokeName(name string) bool {
	if s.dynInvok == nil {
		s.entries(nil) // build the edge/name indexes
	}
	return s.dynInvok[name]
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
	s.sccFns = map[int][]*ssa.Function{}
	next := 0
	for i := len(order) - 1; i >= 0; i-- {
		root := order[i]
		if _, done := s.sccOf[root]; done {
			continue
		}
		id := next
		next++
		s.sccOf[root] = id
		s.sccFns[id] = append(s.sccFns[id], root)
		stack := []*ssa.Function{root}
		for len(stack) > 0 {
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, p := range pred[f] {
				if _, done := s.sccOf[p]; !done {
					s.sccOf[p] = id
					s.sccFns[id] = append(s.sccFns[id], p)
					stack = append(stack, p)
				}
			}
		}
		s.cyclic[id] = len(s.sccFns[id]) > 1
	}
	for fn, out := range succ {
		for _, c := range out {
			if c == fn {
				s.cyclic[s.sccOf[fn]] = true
			}
		}
	}
}
