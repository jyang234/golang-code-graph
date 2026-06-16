// Package features reduces a call-graph edge to the normalized feature vector the
// shared tier-map classifies (static-extractor spec §5). It owns the parsed
// classification hints — which packages are the logger, the bus, the DB, the
// outbound HTTP seam — and the rules that turn a callee plus its call site into a
// Boundary/Effect/Origin/Fallible/Concurrent tuple.
//
// Effect is set honestly: mutate/read are known only at the boundary via hints
// (db.Exec→mutate, db.Query→read), an outbound call to a peer service is io (not
// read, so it tiers as ext-sync = 1, while a DB read tiers as ext-read = 2), and
// first-party internals fall to compute → tier 3.
package features

import (
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/model"
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
		Concurrent: isConcurrentSite(site),
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
		f.Boundary, f.Effect = model.BoundaryOutboundSync, dbEffect(callee)
	default:
		f.Boundary, f.Effect = e.structural(caller, callee), model.EffectCompute
	}
	return f
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
	if isStdlib(cp) {
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

// dbEffect maps a DB method to its effect by name: Query*→read, Exec*→mutate.
func dbEffect(fn *ssa.Function) model.Effect {
	switch {
	case strings.HasPrefix(fn.Name(), "Query"):
		return model.EffectRead
	case strings.HasPrefix(fn.Name(), "Exec"):
		return model.EffectMutate
	default:
		return model.EffectIO
	}
}

// isConcurrentSite reports whether the call is a `go` or `defer` dispatch. This is
// the direct SSA signal; a closure dispatched concurrently by a library such as
// errgroup is not detected here (the behavioral pipeline owns runtime concurrency).
func isConcurrentSite(site ssa.CallInstruction) bool {
	switch site.(type) {
	case *ssa.Go, *ssa.Defer:
		return true
	default:
		return false
	}
}

// Fallible reports whether fn returns or propagates an error.
func Fallible(fn *ssa.Function) bool { return fn != nil && returnsError(fn.Signature) }

// returnsError reports whether sig has an error result.
func returnsError(sig *types.Signature) bool {
	if sig == nil {
		return false
	}
	res := sig.Results()
	for i := 0; i < res.Len(); i++ {
		if types.Identical(res.At(i).Type(), errorType) {
			return true
		}
	}
	return false
}

var errorType = types.Universe.Lookup("error").Type()

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

// isStdlib reports whether an import path is a standard-library package: its first
// path segment contains no dot (so "net/http" and "database/sql" are stdlib,
// "golang.org/x/sync" is not).
func isStdlib(pkgPath string) bool {
	seg := pkgPath
	if i := strings.IndexByte(pkgPath, '/'); i >= 0 {
		seg = pkgPath[:i]
	}
	return !strings.Contains(seg, ".")
}
