// Package taint is flowmap's sound forward value-flow (taint) analysis: it answers,
// for a declared set of sensitive SOURCES and must-not-receive SINKS, whether
// sensitive data can flow from a source to a sink argument. It is the substrate for
// a "this PII field cannot reach this log/boundary" gate
// (docs/design/flowmap-capability-headroom.md §3).
//
// The model is reachability's asymmetry, the same shape as the rest of flowmap:
//
//   - FLOW    — a source value reaches a sink argument: a could-flow CANDIDATE to
//     verify (over-approximate, so it may over-report).
//   - NO-FLOW — PROVEN no source reaches any sink: a sound proof, emitted ONLY when
//     the forward over-approximation was COMPLETE (nothing escaped view).
//   - ABSTAIN — taint flowed into a construct the analysis does not model (a map,
//     an interface box, a channel, a closure capture, or a call into code
//     with no analyzable body), so completeness is lost. We refuse to
//     claim NO-FLOW — never a false no-flow.
//
// Soundness is the whole point: a false NO-FLOW is a false SATISFIED, the worst
// outcome. So taint propagates OVER-approximately (forward def-use, struct fields by
// a global (type,field) set, interprocedural arg→param and return), and ANY value
// that escapes a modeled construct sets the escaped flag, which downgrades a
// would-be NO-FLOW to ABSTAIN. The analysis descends only into first-party function
// bodies; taint passed into non-first-party code (other than a declared sink) is an
// escape, not a silent no-op.
//
// Scope (v1, a measured spike — precision is a follow-up, not assumed): no
// go/pointer; maps/interfaces/channels/closures/reflection are the abstain frontier;
// the escaped flag is per-analysis (one escape downgrades the whole no-flow claim).
package taint

import (
	"go/token"
	"go/types"
	"sort"

	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/static/features"
	"github.com/jyang234/golang-code-graph/internal/static/ssabuild"
)

// Verdict is the three-valued result, in flowmap's PROVEN/VIOLATED/CANT-PROVE register.
type Verdict string

const (
	Flow    Verdict = "FLOW"    // a source reaches a sink arg (could-flow candidate) — VIOLATED
	NoFlow  Verdict = "NO-FLOW" // proven no source reaches a sink (sound) — PROVEN
	Abstain Verdict = "ABSTAIN" // taint escaped a modeled construct — CANT-PROVE
)

// FuncSpec matches a function by package path and name (the classify-hint shape):
// "importpath#Name". A source FuncSpec taints the call RESULT; a sink FuncSpec
// flags any tainted ARGUMENT.
type FuncSpec struct {
	Pkg  string
	Name string
}

func (s FuncSpec) matches(fn *ssa.Function) bool {
	return fn != nil && features.PkgPath(fn) == s.Pkg && fn.Name() == s.Name
}

// FieldSpec marks reads of a sensitive struct field as a source: "importpath.Type#Field".
type FieldSpec struct {
	Pkg   string
	Type  string
	Field string
}

// Config is the declared source/sink set the analysis runs against.
type Config struct {
	SourceFuncs  []FuncSpec
	SourceFields []FieldSpec
	Sinks        []FuncSpec
}

// Finding is one could-flow: a sink call whose argument carries source taint.
type Finding struct {
	Sink string // the sink function (pkg#Name)
	Site string // FQN of the first-party function where the sink is called
}

// Report is the analysis result.
type Report struct {
	Verdict     Verdict
	Flows       []Finding
	Escaped     bool
	EscapeSites []string // FQNs where taint escaped a modeled construct (disclosure)
	Sources     int      // source seeds matched (func results + field reads)
}

// fieldKey identifies a struct field globally: (named type, field index). Taint on a
// field is type+index-global (field-INsensitive to instance) — the same trick the
// SQL fold uses; an over-approximation in the safe direction (it can only over-taint).
type fieldKey struct {
	named *types.Named
	idx   int
}

// analysis holds the worklist state for one Analyze run.
type analysis struct {
	cfg   Config
	funcs []*ssa.Function // first-party functions (the descent scope)
	prog  *ssabuild.Program

	tainted      map[ssa.Value]bool
	taintedField map[fieldKey]bool
	queue        []ssa.Value

	// fieldLoads maps a field to every value that loads it, so tainting a field taints
	// its reads program-wide (over-approximate completeness). Precomputed once.
	fieldLoads map[fieldKey][]ssa.Value
	// callsTo maps a first-party callee to the call instructions that invoke it, so a
	// taint-returning function taints its callers' results (return-flow). Precomputed.
	callsTo map[*ssa.Function][]ssa.CallInstruction

	retTainted map[*ssa.Function]bool

	flows       []Finding
	flowSeen    map[Finding]bool
	escaped     bool
	escapeSites map[string]bool
}

// Analyze runs the forward taint analysis over prog's first-party code and returns
// the trichotomy report.
func Analyze(prog *ssabuild.Program, cfg Config) Report {
	a := &analysis{
		cfg:          cfg,
		prog:         prog,
		tainted:      map[ssa.Value]bool{},
		taintedField: map[fieldKey]bool{},
		fieldLoads:   map[fieldKey][]ssa.Value{},
		callsTo:      map[*ssa.Function][]ssa.CallInstruction{},
		retTainted:   map[*ssa.Function]bool{},
		flowSeen:     map[Finding]bool{},
		escapeSites:  map[string]bool{},
	}
	a.funcs = firstPartyFuncs(prog)
	a.index()
	sources := a.seed()
	a.run()

	r := Report{
		Flows:   a.flows,
		Escaped: a.escaped,
		Sources: sources,
	}
	for s := range a.escapeSites {
		r.EscapeSites = append(r.EscapeSites, s)
	}
	sort.Strings(r.EscapeSites)
	sort.Slice(r.Flows, func(i, j int) bool {
		if r.Flows[i].Sink != r.Flows[j].Sink {
			return r.Flows[i].Sink < r.Flows[j].Sink
		}
		return r.Flows[i].Site < r.Flows[j].Site
	})
	switch {
	case len(r.Flows) > 0:
		r.Verdict = Flow
	case r.Escaped:
		r.Verdict = Abstain
	default:
		r.Verdict = NoFlow
	}
	return r
}

// index precomputes the field-load and caller indexes over first-party code.
func (a *analysis) index() {
	for _, fn := range a.funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				switch x := instr.(type) {
				case *ssa.FieldAddr:
					if k, ok := fieldKeyOf(x.X.Type(), x.Field); ok {
						for _, ref := range refs(x) {
							if u, ok := ref.(*ssa.UnOp); ok && u.Op == token.MUL {
								a.fieldLoads[k] = append(a.fieldLoads[k], u)
							}
						}
					}
				case *ssa.Field:
					if k, ok := fieldKeyOf(x.X.Type(), x.Field); ok {
						a.fieldLoads[k] = append(a.fieldLoads[k], x)
					}
				}
				if call, ok := instr.(ssa.CallInstruction); ok {
					if callee := call.Common().StaticCallee(); callee != nil {
						a.callsTo[callee] = append(a.callsTo[callee], call)
					}
				}
			}
		}
	}
}

// seed taints the source call-results and source field reads, returning how many
// seeds matched.
func (a *analysis) seed() int {
	n := 0
	// Source functions: the result of every call to one is tainted.
	for _, fn := range a.funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				call, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				callee := call.Common().StaticCallee()
				if callee == nil || !a.isSource(callee) {
					continue
				}
				if v := callResult(call); v != nil {
					a.taint(v)
					n++
				}
			}
		}
	}
	// Source fields: mark the (type,field) tainted so every read is tainted.
	for k := range a.fieldLoads {
		if a.isSourceField(k) {
			a.taintField(k)
			n++
		}
	}
	return n
}

// run drives the worklist to a fixpoint.
func (a *analysis) run() {
	for len(a.queue) > 0 {
		v := a.queue[len(a.queue)-1]
		a.queue = a.queue[:len(a.queue)-1]
		a.propagate(v)
	}
}

// propagate pushes taint from v to each instruction that uses it, following the
// sound rules; unmodeled destinations set escaped.
func (a *analysis) propagate(v ssa.Value) {
	for _, ref := range refs(v) {
		switch r := ref.(type) {
		case *ssa.UnOp:
			a.taint(r) // load/deref/negate of tainted → tainted
		case *ssa.Phi, *ssa.Convert, *ssa.ChangeType, *ssa.ChangeInterface,
			*ssa.Slice, *ssa.Field, *ssa.Index, *ssa.Extract, *ssa.TypeAssert, *ssa.BinOp:
			a.taint(r.(ssa.Value))
		case *ssa.MakeInterface:
			a.escape(r) // boxing into an interface → dynamic-dispatch frontier
		case *ssa.MakeClosure:
			a.escape(r) // captured by a closure → frontier (v1)
		case *ssa.Store:
			a.handleStore(r, v)
		case *ssa.MapUpdate:
			if r.Value == v || r.Key == v {
				a.escape(r) // value OR key into a map — contents not tracked
			}
		case *ssa.Send:
			if r.X == v {
				a.escape(r) // value into a channel — not tracked
			}
		case *ssa.Return:
			a.handleReturn(r, v)
		case ssa.CallInstruction:
			a.handleCall(r, v)
		default:
			// SOUNDNESS BACKSTOP: any taint-carrying referrer kind not modeled above
			// (indexing a tainted slice via *ssa.IndexAddr, ranging a tainted
			// map/slice via *ssa.Range/*ssa.Next, *ssa.Lookup, *ssa.Select, …) must
			// FAIL CLOSED — escape, never silently drop. Without this, an unhandled
			// kind would propagate no taint AND set no escape, so the verdict could be
			// a false NO-FLOW (a false SATISFIED, the worst outcome). Escaping only
			// ever downgrades NO-FLOW to ABSTAIN; it can never hide a real flow.
			a.escape(ref)
		}
	}
}

// handleStore propagates taint through a store of v into an address.
func (a *analysis) handleStore(s *ssa.Store, v ssa.Value) {
	if s.Val != v {
		return // v is the address, not the stored value
	}
	switch addr := s.Addr.(type) {
	case *ssa.FieldAddr:
		if k, ok := fieldKeyOf(addr.X.Type(), addr.Field); ok {
			a.taintField(k) // a struct field now holds taint — every read of it is tainted
			return
		}
		a.escape(s)
	case *ssa.Alloc:
		a.taint(addr) // a local cell holds taint — its loads (UnOp MUL) become tainted
	case *ssa.IndexAddr:
		a.escape(s) // slice/array element — not tracked
	default:
		a.escape(s) // a global or otherwise unmodeled address
	}
}

// handleReturn marks the enclosing function as taint-returning, then taints the
// results of every call to it (interprocedural return-flow).
func (a *analysis) handleReturn(ret *ssa.Return, v ssa.Value) {
	for _, res := range ret.Results {
		if res != v {
			continue
		}
		fn := ret.Parent()
		if a.retTainted[fn] {
			return
		}
		a.retTainted[fn] = true
		for _, call := range a.callsTo[fn] {
			if cv := callResult(call); cv != nil {
				a.taint(cv)
			}
		}
		return
	}
}

// handleCall propagates taint from an argument into the callee. A declared sink
// records a FLOW finding; a first-party callee gets its parameter tainted
// (arg→param); anything else escapes (taint left analyzable view).
func (a *analysis) handleCall(call ssa.CallInstruction, v ssa.Value) {
	common := call.Common()
	callee := common.StaticCallee()

	// Find the argument position(s) v occupies.
	var argIdx []int
	for i, arg := range common.Args {
		if arg == v {
			argIdx = append(argIdx, i)
		}
	}
	if len(argIdx) == 0 {
		// v is the callee value itself, or unrelated; calling a tainted func value is
		// an unmodeled dispatch.
		if common.Value == v {
			a.escape(call)
		}
		return
	}

	if callee != nil && a.isSink(callee) {
		a.recordFlow(call, callee)
		return // a sink is a known endpoint; no need to descend
	}
	if callee != nil && a.isFirstParty(callee) && len(callee.Blocks) > 0 {
		for _, i := range argIdx {
			if i < len(callee.Params) {
				a.taint(callee.Params[i])
			}
		}
		return
	}
	// Unresolved (interface/func-value) or non-first-party callee: taint left view.
	a.escape(call)
}

func (a *analysis) recordFlow(call ssa.CallInstruction, sink *ssa.Function) {
	f := Finding{
		Sink: sink.Pkg.Pkg.Path() + "#" + sink.Name(),
		Site: call.Parent().RelString(nil),
	}
	if !a.flowSeen[f] {
		a.flowSeen[f] = true
		a.flows = append(a.flows, f)
	}
}

func (a *analysis) taint(v ssa.Value) {
	if v == nil || a.tainted[v] {
		return
	}
	a.tainted[v] = true
	a.queue = append(a.queue, v)
}

func (a *analysis) taintField(k fieldKey) {
	if a.taintedField[k] {
		return
	}
	a.taintedField[k] = true
	for _, load := range a.fieldLoads[k] {
		a.taint(load)
	}
}

func (a *analysis) escape(instr ssa.Instruction) {
	a.escaped = true
	if p := instr.Parent(); p != nil {
		a.escapeSites[p.RelString(nil)] = true
	}
}

func (a *analysis) isSource(fn *ssa.Function) bool { return matchAny(a.cfg.SourceFuncs, fn) }

func (a *analysis) isSink(fn *ssa.Function) bool { return matchAny(a.cfg.Sinks, fn) }

func (a *analysis) isSourceField(k fieldKey) bool {
	obj := k.named.Obj()
	// A named type's TypeName can be package-less (a universe/builtin or a
	// synthesized type reachable through go/ssa); guard the Pkg() deref explicitly
	// (fail closed — such a type cannot match a declared importpath#Type.Field source).
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	path := obj.Pkg().Path()
	name := obj.Name()
	st, ok := k.named.Underlying().(*types.Struct)
	if !ok || k.idx >= st.NumFields() {
		return false
	}
	field := st.Field(k.idx).Name()
	for _, s := range a.cfg.SourceFields {
		if s.Pkg == path && s.Type == name && s.Field == field {
			return true
		}
	}
	return false
}

func (a *analysis) isFirstParty(fn *ssa.Function) bool {
	return fn.Pkg != nil && a.prog.IsFirstParty(fn.Pkg)
}

// --- helpers ---

func matchAny(specs []FuncSpec, fn *ssa.Function) bool {
	for _, s := range specs {
		if s.matches(fn) {
			return true
		}
	}
	return false
}

// refs returns v's referrer instructions (nil-safe).
func refs(v ssa.Value) []ssa.Instruction {
	r := v.Referrers()
	if r == nil {
		return nil
	}
	return *r
}

// callResult returns the value a call instruction produces (the *ssa.Call itself for
// a value call), or nil for a Go/Defer (which produce no value).
func callResult(call ssa.CallInstruction) ssa.Value {
	if c, ok := call.(*ssa.Call); ok {
		return c
	}
	return nil
}

// fieldKeyOf returns the (named struct, field index) key for a FieldAddr/Field whose
// base type is t, or ok=false if t is not a (pointer to a) named struct.
func fieldKeyOf(t types.Type, idx int) (fieldKey, bool) {
	n := structNamed(t)
	if n == nil {
		return fieldKey{}, false
	}
	return fieldKey{named: n, idx: idx}, true
}

// structNamed returns the named struct type behind t (stripping one pointer), or nil.
func structNamed(t types.Type) *types.Named {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	if !ok {
		return nil
	}
	if _, ok := n.Underlying().(*types.Struct); !ok {
		return nil
	}
	return n
}

// firstPartyFuncs collects every first-party SSA function (package-level and nested
// anonymous), the descent scope of the analysis.
func firstPartyFuncs(prog *ssabuild.Program) []*ssa.Function {
	var out []*ssa.Function
	seen := map[*ssa.Function]bool{}
	var add func(fn *ssa.Function)
	add = func(fn *ssa.Function) {
		if fn == nil || seen[fn] || fn.Blocks == nil {
			return
		}
		seen[fn] = true
		out = append(out, fn)
		for _, anon := range fn.AnonFuncs {
			add(anon)
		}
	}
	for _, pkg := range prog.ServicePkgs {
		for _, m := range pkg.Members {
			switch x := m.(type) {
			case *ssa.Function:
				add(x)
			case *ssa.Type:
				// Methods of named types in this package.
				ms := prog.Prog.MethodSets.MethodSet(x.Type())
				for i := 0; i < ms.Len(); i++ {
					add(prog.Prog.MethodValue(ms.At(i)))
				}
			}
		}
	}
	return out
}
