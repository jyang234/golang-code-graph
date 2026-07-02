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
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/jyang234/golang-code-graph/internal/static/callgraph"
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

// SourceReport is one declared source's verdict, from AnalyzeBySource: the analysis
// run scoped to that SINGLE source (against the full declared sink set). Source is
// the canonical descriptor — "importpath#Name" for a func source, "importpath#Type.Field"
// for a field source — so the decomposition is reported in a stable, run-independent order.
type SourceReport struct {
	Source string
	Report Report
}

// SinkReport is one declared sink's verdict, from AnalyzeBySink: the aggregate analysis
// PARTITIONED to that sink. Sink is the canonical descriptor "importpath#Name". The Report
// carries only the flows to this sink, but INHERITS the aggregate's Escaped/EscapeSites
// (the forward cone's completeness is sink-independent), so a sink with no flow reads
// NO-FLOW only when the whole cone was complete.
type SinkReport struct {
	Sink   string
	Report Report
}

// SourceSinkReport is one cell of the full (source × sink) matrix, from
// AnalyzeBySourceAndSink: the per-source run for Source, partitioned to Sink. It answers
// the most specific question — "does THIS field reach THIS sink" — and the cells marginalise
// back to AnalyzeBySource (over sinks) and AnalyzeBySink (over sources).
type SourceSinkReport struct {
	Source string
	Sink   string
	Report Report
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
	// It indexes BOTH static call sites and interface/func-value (invoke) sites
	// resolved by the call graph, so return-flow through an interface method is not
	// silently dropped (C-3).
	callsTo map[*ssa.Function][]ssa.CallInstruction

	// siteCallees maps a call site to its concrete callees as resolved by the RTA/VTA
	// call graph. It is the ONLY way an invoke (interface / func-value) site is
	// attributed to concrete functions — StaticCallee is nil for those. Empty when no
	// call graph was supplied, in which case an invoke site is treated as an
	// un-enumerable frontier and escapes (fail closed → ABSTAIN, never a false
	// NO-FLOW). Precomputed.
	siteCallees map[ssa.CallInstruction][]*ssa.Function

	retTainted map[*ssa.Function]bool

	flows       []Finding
	flowSeen    map[Finding]bool
	escaped     bool
	escapeSites map[string]bool
}

// prepared holds the cfg-INDEPENDENT indexes over prog's first-party code: the function
// set (a whole-program ssautil.AllFunctions walk) and the field-load / caller indexes.
// They are a pure function of prog, so prepare builds them ONCE and analyze reuses them
// for every cfg — which is what lets AnalyzeBySource run per declared source without
// re-walking the whole program each time. None of the shared maps/slices is mutated by a
// run (seed/run only read fieldLoads/callsTo), so sharing them across runs is safe.
type prepared struct {
	prog        *ssabuild.Program
	funcs       []*ssa.Function
	fieldLoads  map[fieldKey][]ssa.Value
	callsTo     map[*ssa.Function][]ssa.CallInstruction
	siteCallees map[ssa.CallInstruction][]*ssa.Function
}

// prepare walks prog's first-party code once and builds the cfg-independent indexes.
// cg (the RTA/VTA call graph) resolves interface/func-value dispatch; a nil cg
// leaves invoke sites un-enumerable, which the analysis treats as an escape
// frontier (fail closed → ABSTAIN, never a false NO-FLOW).
func prepare(prog *ssabuild.Program, cg *callgraph.Graph) *prepared {
	p := &prepared{
		prog:        prog,
		funcs:       firstPartyFuncs(prog),
		fieldLoads:  map[fieldKey][]ssa.Value{},
		callsTo:     map[*ssa.Function][]ssa.CallInstruction{},
		siteCallees: resolveSiteCallees(cg),
	}
	for _, fn := range p.funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				switch x := instr.(type) {
				case *ssa.FieldAddr:
					if k, ok := fieldKeyOf(x.X.Type(), x.Field); ok {
						for _, ref := range refs(x) {
							if u, ok := ref.(*ssa.UnOp); ok && u.Op == token.MUL {
								p.fieldLoads[k] = append(p.fieldLoads[k], u)
							}
						}
					}
				case *ssa.Field:
					if k, ok := fieldKeyOf(x.X.Type(), x.Field); ok {
						p.fieldLoads[k] = append(p.fieldLoads[k], x)
					}
				}
				if call, ok := instr.(ssa.CallInstruction); ok {
					if callee := call.Common().StaticCallee(); callee != nil {
						p.callsTo[callee] = append(p.callsTo[callee], call)
					} else if call.Common().IsInvoke() {
						// Return-flow through an interface method: index the invoke site
						// against each concrete callee the call graph resolved, so a
						// taint-returning method taints THIS site's result too (C-3).
						for _, callee := range p.siteCallees[call] {
							p.callsTo[callee] = append(p.callsTo[callee], call)
						}
					}
				}
			}
		}
	}
	return p
}

// resolveSiteCallees inverts the call graph into a per-call-site concrete-callee
// index (nil-safe). It carries every resolved edge — static and invoke alike — but
// is consulted only where StaticCallee is unavailable (invoke sites).
func resolveSiteCallees(cg *callgraph.Graph) map[ssa.CallInstruction][]*ssa.Function {
	out := map[ssa.CallInstruction][]*ssa.Function{}
	if cg == nil {
		return out
	}
	for _, n := range cg.Nodes {
		for _, e := range n.Out {
			if e.Site == nil || e.Callee == nil || e.Callee.Func == nil {
				continue
			}
			out[e.Site] = append(out[e.Site], e.Callee.Func)
		}
	}
	return out
}

// Analyze runs the forward taint analysis over prog's first-party code and returns
// the trichotomy report. cg (the RTA/VTA call graph) resolves interface/func-value
// dispatch so a flow through an interface method is not silently proven absent
// (C-3/C-4); a nil cg makes every invoke site an escape frontier (ABSTAIN).
func Analyze(prog *ssabuild.Program, cg *callgraph.Graph, cfg Config) Report {
	return prepare(prog, cg).analyze(cfg)
}

// analyze runs the forward taint analysis for one cfg over the prepared (cfg-independent)
// indexes, with its own fresh per-run state.
func (p *prepared) analyze(cfg Config) Report {
	a := &analysis{
		cfg:          cfg,
		prog:         p.prog,
		funcs:        p.funcs,
		fieldLoads:   p.fieldLoads,
		callsTo:      p.callsTo,
		siteCallees:  p.siteCallees,
		tainted:      map[ssa.Value]bool{},
		taintedField: map[fieldKey]bool{},
		retTainted:   map[*ssa.Function]bool{},
		flowSeen:     map[Finding]bool{},
		escapeSites:  map[string]bool{},
	}
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
	r.Verdict = verdictOf(r.Flows, r.Escaped)
	return r
}

// verdictOf is the single trichotomy rule, shared by Analyze and the by-sink / matrix
// decompositions: a recorded flow is FLOW; otherwise an escaped cone cannot prove
// no-flow (ABSTAIN); a complete cone with no flow is the only PROVEN NO-FLOW. Keeping
// it in one place is why a per-sink/per-cell verdict cannot drift from the aggregate's.
func verdictOf(flows []Finding, escaped bool) Verdict {
	switch {
	case len(flows) > 0:
		return Flow
	case escaped:
		return Abstain
	default:
		return NoFlow
	}
}

// AnalyzeBySource decomposes the analysis by DECLARED source: it runs Analyze once
// per source (each scoped to that one source but against the FULL declared sink set)
// and returns one SourceReport per source, in canonical Source order.
//
// This is reporting granularity, NOT a capability change, and it is sound for one
// reason: taint here is a monotone per-value union — a source reaches a sink iff its
// OWN value does, independent of the other declared sources (there is no conjunction;
// a value is tainted if ANY incoming source taints it). So the per-source reports
// union back to EXACTLY Analyze(all sources): decomposing loses nothing (every
// aggregate flow/escape is attributable to some single source whose scoped run also
// finds it) and invents nothing (a source that escapes still ABSTAINs — we never
// manufacture a NO-FLOW the aggregate could not prove). TestAnalyzeBySource_Decomposes
// pins the union identity. The point is that an aggregate FLOW from one source no
// longer MASKS the others' NO-FLOW/ABSTAIN/FLOW status.
func AnalyzeBySource(prog *ssabuild.Program, cg *callgraph.Graph, cfg Config) []SourceReport {
	return prepare(prog, cg).bySource(cfg)
}

// bySource runs the analysis once per declared source over the prepared (shared) indexes
// and returns the SourceReports in canonical Source order. Factored out so Decompose can
// run the per-source pass ONCE and derive both the by-source and the (source × sink)
// decompositions from it.
func (p *prepared) bySource(cfg Config) []SourceReport {
	var out []SourceReport
	for _, fs := range cfg.SourceFuncs {
		out = append(out, SourceReport{
			Source: fs.Pkg + "#" + fs.Name,
			Report: p.analyze(Config{SourceFuncs: []FuncSpec{fs}, Sinks: cfg.Sinks}),
		})
	}
	for _, fs := range cfg.SourceFields {
		out = append(out, SourceReport{
			Source: fs.Pkg + "#" + fs.Type + "." + fs.Field,
			Report: p.analyze(Config{SourceFields: []FieldSpec{fs}, Sinks: cfg.Sinks}),
		})
	}
	// Canonical, run-independent order (the config slices are author-ordered; sorting on
	// the intrinsic Source key keeps the decomposition byte-identical across runs).
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

// AnalyzeBySink decomposes the analysis by DECLARED sink: which sink actually receives
// source data. It needs only ONE analysis — the FULL-sink aggregate — PARTITIONED by sink
// (see BySink). Re-running scoped to a single sink would be UNSOUND-leaning (imprecise):
// the other declared sinks are typically external, so a tainted value reaching one would
// ESCAPE (an undeclared external call) instead of being recorded-and-stopped, spuriously
// downgrading a true NO-FLOW to ABSTAIN. Reported in canonical Sink order.
func AnalyzeBySink(prog *ssabuild.Program, cg *callgraph.Graph, cfg Config) []SinkReport {
	return BySink(Analyze(prog, cg, cfg), cfg.Sinks)
}

// BySink partitions an aggregate Report (run against the FULL sink set) into one SinkReport
// per declared sink — a pure function, NO new analysis. The Escaped/EscapeSites are the
// aggregate's (sink-independent), so a no-flow sink is NO-FLOW only on a complete cone,
// else ABSTAIN. Exported so a caller that already holds the aggregate (the CLI's `rep`)
// derives the per-sink view without re-running Analyze.
func BySink(agg Report, sinks []FuncSpec) []SinkReport {
	seen := map[string]bool{}
	var out []SinkReport
	for _, s := range sinks {
		key := s.Pkg + "#" + s.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		var flows []Finding
		for _, f := range agg.Flows {
			if f.Sink == key {
				flows = append(flows, f)
			}
		}
		out = append(out, SinkReport{Sink: key, Report: Report{
			Verdict:     verdictOf(flows, agg.Escaped),
			Flows:       flows,
			Escaped:     agg.Escaped,
			EscapeSites: agg.EscapeSites,
			Sources:     agg.Sources,
		}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sink < out[j].Sink })
	return out
}

// AnalyzeBySourceAndSink computes the full (source × sink) matrix: for every declared
// source it runs the analysis scoped to that source (against the FULL sink set, so the
// propagation/escape semantics are correct — see AnalyzeBySink) and partitions that run
// by sink. Reported in canonical (Source, Sink) order.
func AnalyzeBySourceAndSink(prog *ssabuild.Program, cg *callgraph.Graph, cfg Config) []SourceSinkReport {
	return BySourceAndSink(AnalyzeBySource(prog, cg, cfg), cfg.Sinks)
}

// BySourceAndSink derives the (source × sink) matrix from already-computed per-source
// reports by partitioning each by sink — a pure function, NO new analysis. Each cell is
// sound by the conjunction of the by-source and by-sink arguments, and the cells
// marginalise back to the per-source reports (collapsing sinks) and to BySink (collapsing
// sources). Exported so the CLI builds the matrix from the same per-source runs it already
// rendered, instead of re-analysing per source.
func BySourceAndSink(bySource []SourceReport, sinks []FuncSpec) []SourceSinkReport {
	var out []SourceSinkReport
	for _, sr := range bySource {
		for _, snk := range BySink(sr.Report, sinks) {
			out = append(out, SourceSinkReport{Source: sr.Source, Sink: snk.Sink, Report: snk.Report})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Sink < out[j].Sink
	})
	return out
}

// Decomposition is the complete taint result: the authoritative aggregate (what the gate
// reads) plus the three decompositions, all derived from ONE prepare + one per-source pass.
type Decomposition struct {
	Aggregate       Report
	BySource        []SourceReport
	BySink          []SinkReport
	BySourceAndSink []SourceSinkReport
}

// Decompose runs the taint analysis and every decomposition from a SINGLE whole-program
// prepare: one aggregate run (the authoritative gate verdict) and one per-source pass, with
// by-sink and the matrix derived as pure partitions. It avoids the redundant aggregate and
// duplicated per-source runs of calling AnalyzeBySink / AnalyzeBySourceAndSink separately.
func Decompose(prog *ssabuild.Program, cg *callgraph.Graph, cfg Config) Decomposition {
	p := prepare(prog, cg)
	agg := p.analyze(cfg)
	bySource := p.bySource(cfg)
	return Decomposition{
		Aggregate:       agg,
		BySource:        bySource,
		BySink:          BySink(agg, cfg.Sinks),
		BySourceAndSink: BySourceAndSink(bySource, cfg.Sinks),
	}
}

// seed taints the source call-results and source field reads, returning how many
// seeds matched.
func (a *analysis) seed() int {
	n := 0
	// Source functions: the result of every call to one is tainted. Match at each
	// call site over its concrete callees — StaticCallee for a direct call, and the
	// call graph's resolved callees for an interface/func-value (invoke) site, so a
	// declared source invoked through an interface is still seeded (C-4).
	for _, fn := range a.funcs {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				call, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				for _, callee := range a.calleesOf(call) {
					if a.isSource(callee) {
						if v := callResult(call); v != nil {
							a.taint(v)
							n++
						}
						break
					}
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
			// Also taint the enclosing struct value: a value carrying a tainted field,
			// passed WHOLE to an unmodeled callee (fmt.Println(r)), must escape — the
			// (type,field) key alone leaves the whole-struct hand-off invisible (C-5).
			// Over-tainting the container only costs precision (the permitted direction).
			a.taint(addr.X)
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

// handleCall propagates taint from an argument into the callee(s). A declared sink
// records a FLOW finding; a first-party callee gets its parameter tainted
// (arg→param); anything else escapes (taint left analyzable view). For an invoke
// (interface/func-value) site the callee set comes from the call graph; an
// un-enumerable invoke (no resolved callees) escapes — fail closed → ABSTAIN (C-3).
func (a *analysis) handleCall(call ssa.CallInstruction, v ssa.Value) {
	common := call.Common()

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

	callees := a.calleesOf(call)
	if len(callees) == 0 {
		// A statically-resolved call to a bodiless/non-first-party function, or an
		// invoke the call graph could not enumerate: taint leaves the analyzable view.
		a.escape(call)
		return
	}
	escapes := false
	for _, callee := range callees {
		switch {
		case a.isSink(callee):
			a.recordFlow(call, callee)
		case a.isFirstParty(callee) && len(callee.Blocks) > 0:
			for _, i := range argIdx {
				if i < len(callee.Params) {
					a.taint(callee.Params[i])
				}
			}
		default:
			// A non-first-party or bodiless callee (including one leg of a
			// polymorphic invoke): taint left view.
			escapes = true
		}
	}
	if escapes {
		a.escape(call)
	}
}

// calleesOf returns the concrete callees of a call site: the single StaticCallee
// for a direct call, else the call graph's resolved callees for an invoke
// (interface / func-value) site. Empty for an invoke the graph could not resolve
// (or when no graph was supplied) — the caller treats that as an escape frontier.
func (a *analysis) calleesOf(call ssa.CallInstruction) []*ssa.Function {
	if callee := call.Common().StaticCallee(); callee != nil {
		return []*ssa.Function{callee}
	}
	return a.siteCallees[call]
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
	// IsFirstPartyFunc resolves the nil-fn.Pkg synthetics (generic instances,
	// $bound/$thunk wrappers); keying on fn.Pkg alone would treat a first-party
	// generic helper as external and escape into it — a NO-FLOW that should have
	// descended (C-1).
	return a.prog.IsFirstPartyFunc(fn)
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

// firstPartyFuncs collects every first-party SSA function — package-level funcs,
// value- AND pointer-receiver methods, synthetic wrappers, and nested closures — as
// the descent/seeding scope. It uses ssautil.AllFunctions (the complete whole-program
// walk) rather than a hand-rolled pkg.Members / MethodSet(T) walk: MethodSet(T) omits
// pointer-receiver (*T) methods, and a source/sink read located only inside a *T
// method that went un-indexed is a false NO-FLOW — the one verdict this analysis must
// never emit (CLAUDE.md "Sound scope — collect functions completely").
func firstPartyFuncs(prog *ssabuild.Program) []*ssa.Function {
	var out []*ssa.Function
	for fn := range ssautil.AllFunctions(prog.Prog) {
		if fn.Blocks == nil {
			continue // no body (external or abstract) — nothing to scan
		}
		// IsFirstPartyFunc resolves the nil-fn.Pkg synthetics (generic instances,
		// $bound/$thunk wrappers). A source/sink read located only inside a first-party
		// generic instance that went un-scanned is a false NO-FLOW (C-1) — the same
		// under-collection class as the omitted *T methods this walk already guards.
		if prog.IsFirstPartyFunc(fn) {
			out = append(out, fn)
		}
	}
	return out
}
