// Package features reduces a call-graph edge to the normalized feature vector the
// shared tier-map classifies (static-extractor spec §5). It owns the parsed
// classification hints — which packages are the logger, the bus, the DB, the
// outbound HTTP seam — and the rules that turn a callee plus its call site into a
// Boundary/Effect/Origin/Fallible/Concurrent tuple.
//
// Effect is set honestly: a DB call's mutate/read is read off the SQL VERB of a
// constant statement (and fails closed to io when the statement is not constant,
// never asserting a read it cannot prove); an outbound call to a peer service is
// io (not read, so it tiers as ext-sync = 1, while a DB read tiers as ext-read =
// 2), and first-party internals fall to compute → tier 3.
package features

import (
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/canon/sql"
	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/model"
	"github.com/jyang234/golang-code-graph/internal/sqlverb"
	"github.com/jyang234/golang-code-graph/internal/tiermap"
)

// Extractor derives features and tiers for one analyzed program. It bundles the
// hint set, the module path (to tell first-party from dependency), and the
// configured classifier.
type Extractor struct {
	hints      *HintSet
	modulePath string
	classifier *tiermap.Classifier
}

// NewExtractor builds an Extractor from the service config and module path. A nil
// config yields defaults.
func NewExtractor(cfg *config.Config, modulePath string) *Extractor {
	return &Extractor{
		hints:      NewHintSet(cfg),
		modulePath: modulePath,
		classifier: tiermap.New(cfg),
	}
}

// Hints exposes the parsed hint set so boundary/blind-spot extraction can ask the
// same questions ("is this a publish?") features asks.
func (e *Extractor) Hints() *HintSet { return e.hints }

// Classify returns the tier (and deciding rule) for a feature vector.
func (e *Extractor) Classify(f model.Features) (int, string) { return e.classifier.Classify(f) }

// Edge derives the features of the call caller→callee at site.
func (e *Extractor) Edge(caller, callee *ssa.Function, site ssa.CallInstruction) model.Features {
	f := model.Features{
		Identity:   callee.RelString(nil),
		Origin:     e.origin(caller, callee),
		Fallible:   returnsError(callee.Signature),
		Concurrent: IsConcurrentSite(site),
	}
	switch {
	case e.hints.IsTelemetry(callee):
		f.Boundary, f.Effect = model.BoundaryInternal, model.EffectTelemetry
	case e.hints.IsPublish(callee):
		f.Boundary, f.Effect = model.BoundaryOutboundAsync, model.EffectMutate
	case e.hints.IsConsume(callee):
		// The receive side of the bus. Symmetric to publish (inbound vs
		// outbound-async): consuming an event is a boundary, so it tiers as inbound
		// (tier 1) instead of falling through to compute and going invisible.
		f.Boundary, f.Effect = model.BoundaryInbound, model.EffectIO
	case e.hints.IsHTTP(callee):
		f.Boundary, f.Effect = model.BoundaryOutboundSync, model.EffectIO
	case e.hints.IsDB(callee):
		f.Boundary, f.Effect = model.BoundaryOutboundSync, dbEffect(callee, site)
	case methodNamedOutbound(e.hints, callee):
		// A method-named outbound effect (object storage, cache, non-HTTP RPC) is an
		// outbound-sync external effect like HTTP. Its write-ness is not read from the
		// method name (no sound verb), so EffectIO — the budget discloses it as
		// unenforceable rather than guessing a mutation.
		f.Boundary, f.Effect = model.BoundaryOutboundSync, model.EffectIO
	default:
		f.Boundary, f.Effect = e.structural(caller, callee), model.EffectCompute
	}
	return f
}

// methodNamedOutbound reports whether callee is a method-named outbound effect
// (object storage, cache, or non-HTTP RPC) — the kinds whose op is the method name.
func methodNamedOutbound(hints *HintSet, callee *ssa.Function) bool {
	_, ok := hints.MethodNamedOutboundKind(callee)
	return ok
}

// Inbound returns the features of an entry-point operation (an HTTP handler or a
// bus consumer), which is what tiers the contract's entry points.
func (e *Extractor) Inbound(identity string, fallible bool) model.Features {
	return model.Features{Boundary: model.BoundaryInbound, Effect: model.EffectIO, Identity: identity, Fallible: fallible}
}

// Published returns the features of an outbound-async publish of event.
func (e *Extractor) Published(event string) model.Features {
	return model.Features{Boundary: model.BoundaryOutboundAsync, Effect: model.EffectMutate, Identity: event}
}

// External returns the features of an outbound-sync call to a peer service.
func (e *Extractor) External(identity string) model.Features {
	return model.Features{Boundary: model.BoundaryOutboundSync, Effect: model.EffectIO, Identity: identity}
}

// origin classifies where callee lives relative to caller and the module.
func (e *Extractor) origin(caller, callee *ssa.Function) model.Origin {
	cp := PkgPath(callee)
	if cp == "" {
		return model.OriginUnknown
	}
	if PkgPath(caller) == cp {
		return model.OriginSamePackage
	}
	if e.isFirstParty(cp) {
		return model.OriginFirstParty
	}
	if IsStdlib(cp) {
		return model.OriginStdlib
	}
	return model.OriginThirdParty
}

// structural classifies a non-boundary call by package relationship.
func (e *Extractor) structural(caller, callee *ssa.Function) model.Boundary {
	if PkgPath(caller) == PkgPath(callee) {
		return model.BoundaryInternal
	}
	return model.BoundaryCrossPackage
}

func (e *Extractor) isFirstParty(pkgPath string) bool {
	return e.modulePath != "" &&
		(pkgPath == e.modulePath || strings.HasPrefix(pkgPath, e.modulePath+"/"))
}

// dbEffect classifies a DB boundary call's effect from the SQL VERB when the
// statement is a compile-time constant, and fails closed otherwise. The driver
// method name alone is NOT a sound signal: Postgres `INSERT … RETURNING` rides
// QueryContext, so a Query* method can mutate. A read (EffectRead → the lower
// ext-read tier) is therefore asserted ONLY when a constant statement's verb is
// SELECT. A known non-SELECT non-mutating verb is io (not a read assertion); a
// mutating verb is mutate. When the statement is not constant the verb is
// unknown, so it falls back to the method-name HINT but still never asserts a
// read — Exec* mutates, everything else (Query* included) is io. This mirrors how
// the write surface (budget.go) treats an unreadable Query* as "might mutate"
// rather than a proven read.
func dbEffect(callee *ssa.Function, site ssa.CallInstruction) model.Effect {
	if op := constSQLOp(site); op != "" {
		switch {
		case sqlverb.Mutating(op):
			return model.EffectMutate
		case op == "SELECT":
			return model.EffectRead
		default:
			return model.EffectIO
		}
	}
	if strings.HasPrefix(callee.Name(), "Exec") {
		return model.EffectMutate
	}
	return model.EffectIO
}

// constSQLOp returns the upper-cased SQL verb of the call's statement argument
// when it is a compile-time constant, else "". It reads the statement through
// the SAME canonical normalizer (canon/sql) the op key uses, so the verb dbEffect
// classifies on cannot drift from the rendered DB op.
func constSQLOp(site ssa.CallInstruction) string {
	args := StringArgs(site)
	if len(args) >= 1 {
		if stmt, ok := ConstString(args[0]); ok {
			return sql.Normalize(stmt).Operation
		}
	}
	return ""
}

// IsConcurrentSite reports whether the call is a `go` dispatch — the direct SSA
// signal for a concurrently-executing (potentially racing) call. A `defer` is
// NOT concurrent: it runs synchronously at function exit on the same goroutine,
// so feeding it to the no_concurrent_reach gate as a racy edge would produce a
// false Violation. A closure dispatched concurrently by a library such as
// errgroup is also not detected here (the behavioral pipeline owns runtime
// concurrency).
//
// Exported as the single source of truth for "is a goroutine launch": the
// per-edge Concurrent flag (here) and the blindspots ConcurrentDispatch shape
// both read it, so the two cannot drift on what counts as a `go` site.
func IsConcurrentSite(site ssa.CallInstruction) bool {
	_, ok := site.(*ssa.Go)
	return ok
}

// Fallible reports whether fn returns or propagates an error.
func Fallible(fn *ssa.Function) bool { return fn != nil && returnsError(fn.Signature) }

// IsPackageInit reports whether fn is a package initializer — the synthesized
// `init` that runs package-level var inits and the explicit init() funcs. SSA
// names that one exactly "init" with no receiver; user-written init() funcs are
// renamed init#1, init#2, … and a free function cannot be named init, so this
// matches only the init-ordering plumbing, never a real init body that performs a
// boundary call. It is the ONE predicate the static front-end uses to recognize a
// package initializer (roots seeds it into RTA for registration recovery; graphio
// and blindspots exclude it from the rendered service graph and its disclosures).
func IsPackageInit(fn *ssa.Function) bool {
	return fn != nil && fn.Name() == "init" && fn.Signature != nil && fn.Signature.Recv() == nil
}

// returnsError reports whether sig has an error result. A result counts when its
// type IMPLEMENTS error — not only the bare `error` interface but a concrete
// error type like *pkg.TxError — so fallibility here agrees with the obligations
// / effect-order surface (obligations.isErrorType, also types.Implements). Exact-
// identity matching would under-report fallibility for concrete error returns and
// make the two trusted surfaces disagree on whether a function can fail.
func returnsError(sig *types.Signature) bool {
	if sig == nil {
		return false
	}
	res := sig.Results()
	for i := 0; i < res.Len(); i++ {
		if types.Implements(res.At(i).Type(), errorInterface) {
			return true
		}
	}
	return false
}

var errorInterface = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

// PkgPath returns fn's defining package path, or "" for a synthetic function
// (nil ssa Pkg). It is the single source of truth for package attribution shared
// by blindspots and obligations — a fn==nil/Pkg==nil/Pkg.Pkg==nil guard so no
// caller has to re-derive (and drift on) the nil cases.
func PkgPath(fn *ssa.Function) string {
	if fn == nil || fn.Pkg == nil || fn.Pkg.Pkg == nil {
		return ""
	}
	return fn.Pkg.Pkg.Path()
}

// NamedTypeIs reports whether named is the DEFINED type pkgPath.name — the nil-safe
// identity compare shared by the call sites that need it (the SQL-fold receiver match in
// sqlfold and the blind-spot benign-func tier in blindspots), so the Obj()/Pkg() nil
// guard lives in ONE place instead of a hand-kept copy per site that could drift on a
// future nil-safety fix (CLAUDE.md "one source of truth"). The caller resolves named from
// its value FIRST as its context requires — stripping a pointer (sqlfold), or
// types.Unalias'ing an alias (blindspots) — since which resolution is sound differs by
// site; this helper only compares. A nil named (the value was not a defined type) is not
// a match.
func NamedTypeIs(named *types.Named, pkgPath, name string) bool {
	if named == nil {
		return false
	}
	obj := named.Obj()
	return obj != nil && obj.Pkg() != nil &&
		obj.Pkg().Path() == pkgPath && obj.Name() == name
}

// IsStdlib reports whether an import path is a standard-library package: its first
// path segment contains no dot (so "net/http" and "database/sql" are stdlib,
// "golang.org/x/sync" is not). Exported as the single source of truth for the
// stdlib/third-party split shared by the Origin classifier (here) and the
// blindspots ExternalBoundaryCall detector, so the two cannot disagree on what
// counts as a third-party dependency boundary.
func IsStdlib(pkgPath string) bool {
	seg := pkgPath
	if i := strings.IndexByte(pkgPath, '/'); i >= 0 {
		seg = pkgPath[:i]
	}
	return !strings.Contains(seg, ".")
}
