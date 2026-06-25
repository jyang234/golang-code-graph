// Package obligations evaluates domain-declared path obligations over each
// function's SSA control-flow graph — the intraprocedural-but-domain-specific
// slice no off-the-shelf analyzer can know (path-obligations plan). flowmap
// already holds every function's CFG (ssa.Function.Blocks); this package reads
// the intraprocedural structure the call graph discards.
//
// Two obligation kinds, both pure functions of (rules, SSA):
//
//   - must-release: after an acquire, every path to function exit must hit a
//     release. Not a dominance query — a forward CFG walk asking "is any exit
//     reachable from the acquire without passing a release site?", with two
//     refinements: a `defer` of a release covers every later exit on its path,
//     and the acquire's own failure branch (`if err != nil { return err }` on
//     the acquire's error result) is pruned — a failed acquire holds nothing.
//   - must-precede: every Before site must be dominated by a Require site —
//     a straight dominator-tree query (same block: earlier index; otherwise
//     strict block dominance).
//
// The check is value-blind by design: it proves the SHAPE of the lifecycle
// (a release call appears on every path), not that the right value was
// released — the mode-2 wall. Consequently a release performed inside an
// unlisted helper reports VIOLATED; the fix is naming the helper as a release
// ref, keeping the rule vocabulary explicit. Abstention (CANT-PROVE) fires
// when the shape claim itself would be unsound: the acquired value's ownership
// leaves the function (returned, stored, captured by a closure, handed to a
// goroutine), or the function uses recover (control can rejoin invisibly).
// A CANT-PROVE is a disclosed blind spot, never a silent pass. Arbitrary
// (even irreducible) CFGs are fine: the walk does not rely on reducibility,
// and SSA's dominator tree is defined for any CFG.
//
// Verdicts are three-valued plus one disclosure: SATISFIED / VIOLATED /
// CANT-PROVE per anchored site, and UNMATCHED for a rule whose anchor matches
// no call site anywhere — an inert guardrail the reviewer must see.
package obligations

import (
	"fmt"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/static/features"
)

// Status is the verdict for one anchored site (or, for Unmatched, one rule).
type Status string

const (
	// Satisfied: the obligation holds on every modeled path — the universal
	// proof, the class a test suite cannot produce.
	Satisfied Status = "SATISFIED"
	// Violated: a path exists in the CFG where the obligation fails. Syntactic:
	// the path is present; feasibility is not proven.
	Violated Status = "VIOLATED"
	// CantProve: the shape claim would be unsound here; the reason is disclosed.
	CantProve Status = "CANT-PROVE"
	// Unmatched: the rule's anchor matches no call site in the analyzed unit —
	// the guardrail is inert and must not be mistaken for protection.
	Unmatched Status = "UNMATCHED"
)

// Finding is one obligation verdict. Identity is (Rule, Fn, Site); Detail is
// presentation only (D-OB6). Sites are emitted relative to the service
// directory with forward slashes so output is byte-identical across machines.
type Finding struct {
	Rule   string `json:"rule"`
	Kind   string `json:"kind"`
	Fn     string `json:"fn,omitempty"`
	Site   string `json:"site,omitempty"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Check evaluates every rule against every function, returning findings sorted
// by (rule, fn, site). baseDir anchors site paths (the service directory);
// empty means bare file names. sums is the interprocedural summary engine
// (correctness plan CX-2); nil keeps every verdict intraprocedural — the
// summaries-off half of the O-CX2 monotonicity check. The result is a pure
// function of its inputs.
func Check(rules []config.ObligationRule, fns []*ssa.Function, baseDir string, sums *Summaries) []Finding {
	var out []Finding
	for i := range rules {
		rule := &rules[i]
		matched := false
		for _, fn := range fns {
			if fn == nil || len(fn.Blocks) == 0 {
				continue
			}
			var fs []Finding
			if rule.Kind() == config.KindMustRelease {
				fs = checkRelease(rule, fn, baseDir, sums)
			} else {
				fs = checkPrecede(rule, fn, baseDir, sums)
			}
			if len(fs) > 0 {
				matched = true
				out = append(out, fs...)
			}
		}
		if !matched {
			out = append(out, Finding{
				Rule: rule.Name, Kind: rule.Kind(), Status: Unmatched,
				Detail: fmt.Sprintf("anchor %s matches no call site — the rule is inert", anchor(rule)),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		if a.Fn != b.Fn {
			return a.Fn < b.Fn
		}
		return a.Site < b.Site
	})
	return out
}

func anchor(rule *config.ObligationRule) string {
	if rule.Kind() == config.KindMustRelease {
		return rule.Acquire
	}
	return rule.Before
}

// ---- reference matching -----------------------------------------------------

// ref is a parsed "import/path#Symbol" function reference. It matches by
// package path and name (receiver-agnostic), on both static call targets and
// interface-method (invoke) call sites — acquiring through an interface is
// idiomatic Go, and a rule must bind either way.
type ref struct{ pkg, name string }

func parseRef(s string) ref {
	i := strings.IndexByte(s, '#')
	return ref{pkg: s[:i], name: s[i+1:]}
}

func parseRefs(ss []string) []ref {
	out := make([]ref, len(ss))
	for i, s := range ss {
		out[i] = parseRef(s)
	}
	return out
}

func (r ref) matchesCall(c ssa.CallInstruction) bool {
	common := c.Common()
	if common.IsInvoke() {
		m := common.Method
		return m.Pkg() != nil && m.Pkg().Path() == r.pkg && m.Name() == r.name
	}
	callee := common.StaticCallee()
	if callee == nil {
		return false
	}
	return pkgPathOf(callee) == r.pkg && callee.Name() == r.name
}

func anyRef(rs []ref, c ssa.CallInstruction) bool {
	for _, r := range rs {
		if r.matchesCall(c) {
			return true
		}
	}
	return false
}

// pkgPathOf returns fn's defining package path. It delegates to features.PkgPath
// for the common case (one source of truth, nil-safe) and adds a types.Object
// fallback that the shared helper deliberately omits: an obligation FQN still
// needs a package path for a synthetic function (a wrapper/instantiation whose
// ssa Pkg is nil but whose Object resolves), whereas blindspots/features must
// stay "" for synthetics. Keeping the fallback here — and only here — names that
// divergence instead of silently re-deriving the nil cases.
func pkgPathOf(fn *ssa.Function) string {
	if p := features.PkgPath(fn); p != "" {
		return p
	}
	if fn != nil {
		if obj := fn.Object(); obj != nil && obj.Pkg() != nil {
			return obj.Pkg().Path()
		}
	}
	return ""
}

// ---- must-release -----------------------------------------------------------

func checkRelease(rule *config.ObligationRule, fn *ssa.Function, baseDir string, sums *Summaries) []Finding {
	acquire := parseRef(rule.Acquire)
	releases := parseRefs(rule.Release)
	var relKey targetKey
	if sums != nil {
		relKey = sums.intern(rule.Release)
	}

	var acquires []*ssa.Call
	for _, b := range fn.Blocks {
		for _, in := range b.Instrs {
			if call, ok := in.(*ssa.Call); ok && acquire.matchesCall(call) {
				acquires = append(acquires, call)
			}
		}
	}
	if len(acquires) == 0 {
		return nil
	}

	fnRecovers := usesRecover(fn) // per-function fact; do not rescan per acquire site

	var out []Finding
	for i, acq := range acquires {
		f := Finding{
			Rule: rule.Name, Kind: config.KindMustRelease,
			Fn: fn.RelString(nil), Site: site(fn, acq.Pos(), baseDir, i),
		}
		switch {
		case fnRecovers:
			f.Status, f.Detail = CantProve, "function uses recover; control flow after a panic is invisible to the CFG"
		default:
			if why, escaped := ownershipEscapes(acq); escaped {
				f.Status, f.Detail = CantProve, why
			} else {
				switch r := leakPath(fn, acq, releases, sums, relKey); {
				case r.leaked:
					f.Status = Violated
					f.Detail = fmt.Sprintf("exit at %s reachable without release", site(fn, r.exit, baseDir, 0))
				case r.unknown != "":
					f.Status = CantProve
					f.Detail = fmt.Sprintf("release may occur inside %s — beyond proof; name it as a release ref to assert it", r.unknown)
				default:
					f.Status = Satisfied
				}
			}
		}
		out = append(out, f)
	}
	return out
}

// usesRecover reports whether a panic in fn can be swallowed: recover stops
// fn's unwinding only when called directly by a function fn DEFERS, so the
// check is defer-rooted — resolve each Defer's target (a named function or a
// closure; StaticCallee handles both) and scan it for a direct recover call.
// A blanket AnonFuncs scan would be both under-broad (defer handlePanic()
// missed) and over-broad (a recover inside a synchronously-called closure
// affects that closure's frame, not fn's). Also scan fn itself: a direct
// recover call is legal (always returns nil) but signals intent the CFG
// cannot model. Disclosed residual: a DYNAMIC deferred func value (e.g.
// `defer cancel()`) cannot be resolved; abstaining there would abstain on
// most real Go, so it is accepted — the same accepted-imprecision register
// as ignoring implicit runtime panics.
func usesRecover(fn *ssa.Function) bool {
	if callsRecoverDirectly(fn) {
		return true
	}
	for _, b := range fn.Blocks {
		for _, in := range b.Instrs {
			d, ok := in.(*ssa.Defer)
			if !ok {
				continue
			}
			if target := d.Common().StaticCallee(); target != nil && callsRecoverDirectly(target) {
				return true
			}
		}
	}
	return false
}

func callsRecoverDirectly(f *ssa.Function) bool {
	for _, b := range f.Blocks {
		for _, in := range b.Instrs {
			if c, ok := in.(ssa.CallInstruction); ok {
				if bi, ok := c.Common().Value.(*ssa.Builtin); ok && bi.Name() == "recover" {
					return true
				}
			}
		}
	}
	return false
}

// valueWeb returns the set of SSA values aliasing root inside its function:
// tuple extracts, phis, value-preserving conversions, and — the case SSA does
// not lift — loads from a local Alloc the value was stored into (named
// results, variables captured by closures). One alias model serves both the
// resource (escape analysis, release credit) and the acquire's error result
// (failure-branch pruning); recognizing aliases by narrow syntactic shape in
// each consumer is how the same value silently stops being tracked.
func valueWeb(root ssa.Value) map[ssa.Value]bool {
	web := map[ssa.Value]bool{}
	var add func(v ssa.Value)
	add = func(v ssa.Value) {
		if web[v] {
			return
		}
		web[v] = true
		refs := v.Referrers()
		if refs == nil {
			return
		}
		for _, in := range *refs {
			switch r := in.(type) {
			case *ssa.Extract, *ssa.Phi, *ssa.MakeInterface, *ssa.ChangeType,
				*ssa.ChangeInterface, *ssa.Convert, *ssa.TypeAssert:
				add(r.(ssa.Value))
			case *ssa.Store:
				if r.Val != v {
					continue
				}
				if alloc, ok := r.Addr.(*ssa.Alloc); ok {
					web[alloc] = true // the slot itself, for capture analysis
					if arefs := alloc.Referrers(); arefs != nil {
						for _, ain := range *arefs {
							if ld, ok := ain.(*ssa.UnOp); ok && ld.Op == token.MUL {
								add(ld)
							}
						}
					}
				}
			}
		}
	}
	add(root)
	return web
}

// ownershipEscapes reports whether the acquired value's lifecycle leaves the
// function: returned, stored to non-local memory, captured by a closure that
// outlives the call, or handed to a goroutine. Passing the value as a plain
// call argument is NOT an escape — the check is value-blind and a callee that
// releases it must be listed as a release ref. A store into a LOCAL slot is
// alias propagation, not escape (the slot's loads join the web); a closure
// capturing the value whose only use is a `defer` in this function stays
// in-frame (leakPath credits its releases).
func ownershipEscapes(acq *ssa.Call) (string, bool) {
	for _, root := range resourceValues(acq) {
		for v := range valueWeb(root) {
			refs := v.Referrers()
			if refs == nil {
				continue
			}
			for _, in := range *refs {
				switch r := in.(type) {
				case *ssa.Return:
					return "acquired value is returned — its lifecycle leaves the function", true
				case *ssa.Store:
					if r.Val != v {
						continue // writing INTO the value, not moving it
					}
					if _, local := r.Addr.(*ssa.Alloc); !local {
						return "acquired value is stored — its lifecycle leaves the function", true
					}
				case *ssa.MakeClosure:
					if !onlyDeferred(r) {
						return "acquired value is captured by a closure — releases there are invisible", true
					}
				case *ssa.Go:
					return "acquired value is handed to a goroutine — concurrent release is out of scope", true
				}
			}
		}
	}
	return "", false
}

// onlyDeferred reports whether every use of a closure is a `defer` in the
// enclosing function — the cleanup idiom. Such a closure runs in this frame
// at exit; any other use (called later, stored, passed, returned, spawned)
// means the capture outlives the analysis and must abstain.
func onlyDeferred(mc *ssa.MakeClosure) bool {
	refs := mc.Referrers()
	if refs == nil || len(*refs) == 0 {
		return false
	}
	for _, in := range *refs {
		if _, ok := in.(*ssa.Defer); !ok {
			return false
		}
	}
	return true
}

// resourceValues returns the SSA values holding the acquire's resource — the
// non-error result components. The error results are deliberately excluded:
// `return err` on the failure branch is not the resource escaping.
func resourceValues(acq *ssa.Call) []ssa.Value {
	sig := acq.Call.Signature()
	if sig == nil || sig.Results().Len() == 0 {
		return nil
	}
	if sig.Results().Len() == 1 {
		if isErrorType(sig.Results().At(0).Type()) {
			return nil
		}
		return []ssa.Value{acq}
	}
	refs := acq.Referrers()
	if refs == nil {
		return nil
	}
	var out []ssa.Value
	for _, in := range *refs {
		if ex, ok := in.(*ssa.Extract); ok && !isErrorType(sig.Results().At(ex.Index).Type()) {
			out = append(out, ex)
		}
	}
	return out
}

// errorInterface is the universe error type, for semantic (not name-string)
// matching: a concrete error type like *pkg.TxError is still an error.
var errorInterface = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

func isErrorType(t types.Type) bool { return types.Implements(t, errorInterface) }

// leakResult is one acquire site's walk outcome: a leak with its witness exit,
// a handoff whose release behavior is beyond proof (CX-1 abstention), or
// neither — covered on every modeled path.
type leakResult struct {
	exit    token.Pos // valid iff leaked
	leaked  bool
	unknown string // non-empty: a handoff callee the walk could not classify
}

// leakPath walks the CFG forward from the acquire, looking for a function exit
// reachable without passing a release site. A release via plain call or defer
// covers the path from that point on; an explicit panic is an exit (an
// uncovered explicit panic leaks); implicit runtime panics are ignored, as
// lifecycle checkers conventionally do. The acquire's own failure branch is
// pruned: an If whose condition tests the acquire's error result against nil
// only has its success arm followed.
//
// CX-1 (D-CX3): when sums is non-nil, a HANDOFF site — a call or defer the
// resource's value web visibly flows into — consults the callees' summaries:
// all ALWAYS-release ⇒ the handoff covers like an inline release (and a
// deferred ALWAYS-release named helper covers later exits, lifting the
// deferReleases ceiling); all NEVER-release ⇒ the walk continues, so a leak
// past it keeps today's VIOLATED with the same witness; anything else ⇒ that
// path can claim neither leak nor proof — recorded as unknown. A VIOLATED is
// reported only off a path whose handoffs were all provably non-releasing, so
// the witness is never weaker than the intraprocedural one (D-CX2).
func leakPath(fn *ssa.Function, acq *ssa.Call, releases []ref, sums *Summaries, relKey targetKey) leakResult {
	errVals := errorValuesOf(acq)

	var web map[ssa.Value]bool
	if sums != nil {
		web = map[ssa.Value]bool{}
		for _, root := range resourceValues(acq) {
			for v := range valueWeb(root) {
				web[v] = true
			}
		}
	}
	// classify inspects a call the resource flows into: covered (every callee
	// ALWAYS releases — an inline release equivalent), or unknown (a named
	// maybe-release, beyond proof either way). Everything else — not a
	// handoff, a builtin, or callees that provably NEVER release — returns
	// the zero values: the walk just keeps going.
	classify := func(in ssa.CallInstruction) (covered bool, unknown string) {
		if sums == nil || !operandInWeb(in, web) {
			return false, ""
		}
		cands, frontier := sums.resolve(in)
		if frontier {
			return false, "<dynamic>"
		}
		if len(cands) == 0 {
			return false, "" // builtin or no callee: not a release vehicle
		}
		always, never := true, true
		var name string
		for _, c := range cands {
			switch sums.dischargeKey(c, relKey) {
			case SummaryAlways:
				never = false
			case SummaryNever:
				always = false
			default:
				always, never = false, false
				if name == "" {
					name = c.RelString(nil)
				}
			}
		}
		switch {
		case always:
			return true, ""
		case never:
			return false, "" // every candidate provably never releases: keep walking
		default:
			if name == "" {
				name = cands[0].RelString(nil) // mixed ALWAYS/NEVER: dispatch-dependent
			}
			return false, name
		}
	}

	res := leakResult{}
	// Two walks with one classifier, because the leak claim and the proof
	// claim treat an unknown handoff differently: the LEAK hunt must stop
	// there (the callee may have released — no witness past it), while the
	// PROOF hunt must walk through it (an unconditional release further down
	// still proves the path, so an early maybe-release must not force
	// abstention). Each mode's "uncovered exit reachable from B" is
	// prefix-independent, so each keeps the usual visited-set memoization.
	run := func(unknownCovers bool) (token.Pos, bool) {
		visited := map[*ssa.BasicBlock]bool{}
		var walk func(b *ssa.BasicBlock, from int) (token.Pos, bool)
		walk = func(b *ssa.BasicBlock, from int) (token.Pos, bool) {
			for i := from; i < len(b.Instrs); i++ {
				switch in := b.Instrs[i].(type) {
				case *ssa.Call:
					if anyRef(releases, in) {
						return token.NoPos, false // released: this path is covered
					}
					covered, unknown := classify(in)
					if covered {
						return token.NoPos, false // proven release in the callee
					}
					if unknown != "" {
						if res.unknown == "" {
							res.unknown = unknown
						}
						if unknownCovers {
							return token.NoPos, false // no leak claim past a maybe-release
						}
					}
				case *ssa.Defer:
					if deferReleases(in, releases) {
						return token.NoPos, false // deferred release covers every later exit
					}
					covered, unknown := classify(in)
					if covered {
						return token.NoPos, false // deferred proven release covers exits
					}
					if unknown != "" {
						if res.unknown == "" {
							res.unknown = unknown
						}
						if unknownCovers {
							return token.NoPos, false
						}
					}
				case *ssa.Return:
					return exitPos(fn, in), true
				case *ssa.Panic:
					return exitPos(fn, in), true
				case *ssa.If:
					if skip, ok := failureBranch(in, errVals); ok {
						next := in.Block().Succs[1-skip]
						if !visited[next] {
							visited[next] = true
							return walk(next, 0)
						}
						return token.NoPos, false
					}
				}
			}
			for _, next := range b.Succs {
				if visited[next] {
					continue
				}
				visited[next] = true
				if pos, leaked := walk(next, 0); leaked {
					return pos, true
				}
			}
			return token.NoPos, false
		}

		blk := acq.Block()
		start := 0
		for i, in := range blk.Instrs {
			if in == acq {
				start = i + 1
				break
			}
		}
		// Seed the acquire block as visited so a loop back-edge cannot re-enter
		// it at index 0 and re-scan the pre-acquire instructions — that would
		// credit a release of the *previous* iteration's resource against this
		// acquisition (a false "covered"). Instructions from start onward and
		// every successor are already explored by this initial walk, so the
		// back-edge yields no genuine new witness.
		visited[blk] = true
		return walk(blk, start)
	}

	if pos, leaked := run(true); leaked {
		res.exit, res.leaked = pos, true
		return res
	}
	if res.unknown != "" {
		// The proof hunt: a path proven covered past every maybe-release
		// clears the abstention; an uncovered exit confirms it.
		if _, uncovered := run(false); !uncovered {
			res.unknown = ""
		}
	}
	return res
}

// operandInWeb reports whether the resource's value web flows into the call:
// among its arguments, or as the invoke receiver — the D-CX3 handoff test.
func operandInWeb(in ssa.CallInstruction, web map[ssa.Value]bool) bool {
	common := in.Common()
	if common.IsInvoke() && web[common.Value] {
		return true
	}
	for _, a := range common.Args {
		if web[a] {
			return true
		}
	}
	return false
}

// deferReleases reports whether a Defer covers the obligation: it defers a
// release directly, or defers an ANONYMOUS closure whose body calls a release
// — the errcheck-clean idiom `defer func() { _ = tx.Rollback() }()`. The
// one-level scan is deliberately limited to closures: a deferred NAMED helper
// that releases must be listed as a release ref, keeping the rule vocabulary
// explicit (the value-blind contract).
func deferReleases(d *ssa.Defer, releases []ref) bool {
	if anyRef(releases, d) {
		return true
	}
	callee := d.Common().StaticCallee()
	if callee == nil || callee.Parent() == nil {
		return false // not an anonymous closure of this function
	}
	for _, b := range callee.Blocks {
		for _, in := range b.Instrs {
			if c, ok := in.(*ssa.Call); ok && anyRef(releases, c) {
				return true
			}
		}
	}
	return false
}

// errorValuesOf collects every SSA value aliasing the acquire's error
// result(s) — the call itself for a single-result error acquire, the
// error-typed Extracts for a tuple, each expanded through valueWeb so the
// failure-branch test is recognized even when err is a named result, is
// captured by an annotating defer (stored to an alloc and reloaded), or
// merges through a phi. The narrow direct-Extract version produced false
// VIOLATED on exactly those idioms.
func errorValuesOf(acq *ssa.Call) map[ssa.Value]bool {
	out := map[ssa.Value]bool{}
	sig := acq.Call.Signature()
	if sig == nil || sig.Results().Len() == 0 {
		return out
	}
	if sig.Results().Len() == 1 {
		if isErrorType(sig.Results().At(0).Type()) {
			for v := range valueWeb(acq) {
				out[v] = true
			}
		}
		return out
	}
	refs := acq.Referrers()
	if refs == nil {
		return out
	}
	for _, in := range *refs {
		if ex, ok := in.(*ssa.Extract); ok && isErrorType(sig.Results().At(ex.Index).Type()) {
			for v := range valueWeb(ex) {
				out[v] = true
			}
		}
	}
	return out
}

// failureBranch recognizes `if <acquireErr> != nil` (or == nil) and returns
// the index of the failed-acquire successor (the arm where the error is
// non-nil), which the walk must not follow: a failed acquire holds no resource.
func failureBranch(ifInstr *ssa.If, errVals map[ssa.Value]bool) (skipSucc int, ok bool) {
	bin, isBin := ifInstr.Cond.(*ssa.BinOp)
	if !isBin || len(errVals) == 0 {
		return 0, false
	}
	xErr, yNil := errVals[bin.X], isNil(bin.Y)
	yErr, xNil := errVals[bin.Y], isNil(bin.X)
	errVsNil := (xErr && yNil) || (yErr && xNil)
	if !errVsNil {
		return 0, false
	}
	switch bin.Op.String() {
	case "!=":
		return 0, true // true arm (Succs[0]) is the failure path
	case "==":
		return 1, true // false arm (Succs[1]) is the failure path
	}
	return 0, false
}

func isNil(v ssa.Value) bool {
	c, ok := v.(*ssa.Const)
	return ok && c.Value == nil
}

// ---- must-precede -----------------------------------------------------------

func checkPrecede(rule *config.ObligationRule, fn *ssa.Function, baseDir string, sums *Summaries) []Finding {
	require := parseRef(rule.Require)
	before := parseRef(rule.Before)

	type sited struct {
		instr ssa.CallInstruction
		block *ssa.BasicBlock
		index int
	}
	var aSites, bSites []sited
	for _, b := range fn.Blocks {
		for i, in := range b.Instrs {
			call, ok := in.(ssa.CallInstruction)
			if !ok {
				continue
			}
			// A Require site must be a plain call: a deferred A runs at exit,
			// AFTER the B it is supposed to precede. A derived A — a plain
			// call that provably executes the require on every path before
			// returning (CX-2) — counts the same way, for every rule: it
			// widens A-site recognition, not the rule's scope (D-CX9).
			if _, plain := in.(*ssa.Call); plain {
				if require.matchesCall(call) ||
					(sums != nil && sums.DerivedRequire(in.(*ssa.Call), rule.Require)) {
					aSites = append(aSites, sited{call, b, i})
				}
			}
			// A Before site is ANY call form: a deferred or spawned B still
			// happens and still needs its A. The registration/spawn point is
			// the site — sound, since an A dominating the registration runs
			// before the deferred B can.
			if before.matchesCall(call) {
				bSites = append(bSites, sited{call, b, i})
			}
		}
	}
	if len(bSites) == 0 {
		return nil
	}

	var out []Finding
	for i, bs := range bSites {
		f := Finding{
			Rule: rule.Name, Kind: config.KindMustPrecede,
			Fn: fn.RelString(nil), Site: site(fn, bs.instr.Common().Pos(), baseDir, i),
		}
		dominated := false
		for _, as := range aSites {
			if (as.block == bs.block && as.index < bs.index) ||
				(as.block != bs.block && as.block.Dominates(bs.block)) {
				dominated = true
				break
			}
		}
		switch {
		case dominated:
			f.Status = Satisfied
		case sums != nil && rule.FromCallers:
			// The guard-intent lift (D-CX7/D-CX9): the require may run in
			// callers; consult entry domination. Trust-monotone by
			// construction — the intraprocedural verdict here was VIOLATED,
			// and the lift can only prove it away or abstain legibly. There
			// is no interprocedural VIOLATED: "provably require-less entry"
			// would overclaim (adversarial review F3), and the rule author
			// opted into caller context, so unprovable entries abstain.
			sum, note := sums.EntryDominatedNote(fn, rule.Require)
			if sum == SummaryAlways {
				f.Status = Satisfied
				f.Detail = fmt.Sprintf("require-covered at every entry; %s", note)
			} else {
				f.Status = CantProve
				f.Detail = fmt.Sprintf("no call to %s dominates this call to %s in this function; %s", rule.Require, rule.Before, note)
			}
		default:
			f.Status = Violated
			f.Detail = fmt.Sprintf("no call to %s dominates this call to %s", rule.Require, rule.Before)
		}
		out = append(out, f)
	}
	return out
}

// ---- positions ---------------------------------------------------------------

// site renders a position as "dir/file.go:NN", relative to baseDir with
// forward slashes — the first source positions ever emitted into graph.json.
// The normalization is TOTAL: no rung of the fallback ladder emits a
// machine-specific path, because Site is finding identity and the output must
// be byte-identical across checkouts. Outside baseDir (a package above the
// service dir, generated code) the portable package-qualified form
// "<pkg-import-path>/<file base>:<line>" is used; an invalid position yields
// a synthetic-but-unique "<pkg>:synthetic#<ordinal>" so identities never
// collapse onto "".
func site(fn *ssa.Function, pos token.Pos, baseDir string, ordinal int) string {
	p := fn.Prog.Fset.Position(pos)
	if !p.IsValid() {
		return fmt.Sprintf("%s:synthetic#%d", pkgPathOf(fn), ordinal)
	}
	// The service-relative path predicate is shared with the graph's node File field
	// (features.RelFile, the one source of truth); pkgPathOf carries the synthetic
	// Object() fallback the graph's PkgPath omits.
	return features.RelFile(p.Filename, baseDir, pkgPathOf(fn)) + ":" + strconv.Itoa(p.Line)
}

// exitPos is the best position for an exit instruction: its own, else the
// enclosing function's.
func exitPos(fn *ssa.Function, in ssa.Instruction) token.Pos {
	if p := in.Pos(); p.IsValid() {
		return p
	}
	return fn.Pos()
}
