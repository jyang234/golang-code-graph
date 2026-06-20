package features

import (
	"go/constant"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/config"
)

// hint matches a function by package path and, optionally, name. A bare path
// (name == "") matches any call into that package.
type hint struct {
	pkgPath string
	name    string
}

func parseHint(s string) hint {
	if i := strings.IndexByte(s, '#'); i >= 0 {
		return hint{pkgPath: s[:i], name: s[i+1:]}
	}
	return hint{pkgPath: s}
}

func parseHints(ss []string) []hint {
	out := make([]hint, 0, len(ss))
	for _, s := range ss {
		if s != "" {
			out = append(out, parseHint(s))
		}
	}
	return out
}

func (h hint) matches(fn *ssa.Function) bool {
	p := PkgPath(fn)
	if p == "" || p != h.pkgPath {
		return false
	}
	return h.name == "" || fn.Name() == h.name
}

// HintSet is the parsed classification hints plus flowmap's built-in defaults
// (the stdlib loggers, the near-ubiquitous zap structured logger, and
// database/sql).
type HintSet struct {
	telemetry  []hint
	busPublish []hint
	busConsume []hint
	db         []hint
	http       []hint
	// externalExempt are package-path prefixes treated as infrastructure/plumbing
	// rather than a dependency boundary, suppressing their ExternalBoundaryCall
	// disclosures. Prefix (not exact-path) so one entry covers a dependency's
	// subpackages — the OTel family is one built-in prefix, not a list per package.
	externalExempt []string
	// externalTrivial are package-path prefixes whose ExternalBoundaryCall is
	// PURE-COMPUTE / framework plumbing (uuid, a router, codegen runtime) rather than
	// an effect-bearing seam. Unlike externalExempt these do NOT suppress the
	// disclosure — the spot is still detected and counted; the prefix only tags it
	// SeverityTrivial so the signal/noise tier can separate the framework noise from
	// the effect-bearing handful (§21.A). Disclosure-only: an over-broad entry
	// mis-prioritizes, it never hides an effect or moves a verdict.
	externalTrivial []string
}

// NewHintSet builds the hint set from cfg (nil => only built-ins).
func NewHintSet(cfg *config.Config) *HintSet {
	hs := &HintSet{
		// zap is the de-facto standard structured logger; recognizing it built-in
		// means a service does not need a per-service telemetry hint just to keep
		// its logging calls out of the tier-1..3 bands.
		telemetry: parseHints([]string{"log", "log/slog", "go.uber.org/zap", "go.uber.org/zap/zapcore"}),
		db:        parseHints([]string{"database/sql"}),
		// OpenTelemetry is observability plumbing already modeled on the behavioral
		// side; its span/attribute/metric calls pepper nearly every instrumented
		// function, so flagging them as ExternalBoundaryCall would bury the genuine
		// seams. Built-in exempt; teams extend via static.externalBoundaryExempt.
		externalExempt: []string{"go.opentelemetry.io/"},
		// Known-benign, near-ubiquitous PURE-COMPUTE / framework dependencies whose
		// handoff carries no downstream first-party effect: UUID generation, the go-chi
		// router's in-process dispatch helpers, and the oapi-codegen request/response
		// runtime. These are the framework noise §21.A names. Deliberately NOT here: a
		// concurrency primitive like x/sync/errgroup — it ORCHESTRATES first-party
		// closures that do reach effects (loansvc's Evaluate$1/$2 hit the payment gateway
		// and the bus through it), so its handoff is effect-bearing, the default. Built-in
		// so the tier works out of the box; teams extend via static.externalBoundaryTrivial.
		// Tagging only — these are still detected and counted (unlike externalExempt,
		// which suppresses).
		externalTrivial: []string{
			"github.com/google/uuid",
			"github.com/go-chi/chi",
			"github.com/oapi-codegen/runtime",
		},
	}
	if cfg != nil {
		hs.telemetry = append(hs.telemetry, parseHints(cfg.Classify.Telemetry)...)
		hs.busPublish = append(hs.busPublish, parseHints(cfg.Classify.BusPublish)...)
		hs.busConsume = append(hs.busConsume, parseHints(cfg.Classify.BusConsume)...)
		hs.db = append(hs.db, parseHints(cfg.Classify.DB)...)
		hs.http = append(hs.http, parseHints(cfg.Classify.HTTP)...)
		hs.externalExempt = append(hs.externalExempt, cfg.Static.ExternalBoundaryExempt...)
		hs.externalTrivial = append(hs.externalTrivial, cfg.Static.ExternalBoundaryTrivial...)
	}
	return hs
}

func anyMatch(hs []hint, fn *ssa.Function) bool {
	for _, h := range hs {
		if h.matches(fn) {
			return true
		}
	}
	return false
}

// IsTelemetry reports whether fn is a logging/telemetry call.
func (hs *HintSet) IsTelemetry(fn *ssa.Function) bool { return anyMatch(hs.telemetry, fn) }

// IsPublish reports whether fn publishes to the bus (outbound-async).
func (hs *HintSet) IsPublish(fn *ssa.Function) bool { return anyMatch(hs.busPublish, fn) }

// IsConsume reports whether fn subscribes a consumer to the bus.
func (hs *HintSet) IsConsume(fn *ssa.Function) bool { return anyMatch(hs.busConsume, fn) }

// dbResultMethods are the database/sql result-handling methods that operate on
// an already-fetched row or result — they perform no database round-trip, so they
// are not boundary calls even though they live in a DB-hinted package. Excluding
// them keeps result decoding (Scan, iteration, teardown, result inspection) out
// of the DB edges, leaving the actual Query*/Exec* round-trips (and Ping/Begin/
// Prepare) as the only DB boundaries. The names are the conventional SQL cursor
// surface, shared by database/sql, pgx, sqlx, and the like.
var dbResultMethods = map[string]bool{
	"Scan": true, "Next": true, "NextResultSet": true, "Close": true,
	"Err": true, "Columns": true, "ColumnTypes": true,
	"RowsAffected": true, "LastInsertId": true,
}

// IsDB reports whether fn is a database call that crosses the boundary (a
// query/exec round-trip), not a local result-cursor method like Scan or Next.
func (hs *HintSet) IsDB(fn *ssa.Function) bool {
	if fn != nil && dbResultMethods[fn.Name()] {
		return false
	}
	return anyMatch(hs.db, fn)
}

// IsHTTP reports whether fn is an outbound HTTP/RPC seam call.
func (hs *HintSet) IsHTTP(fn *ssa.Function) bool { return anyMatch(hs.http, fn) }

// IsExternalBoundaryExempt reports whether fn's package is exempt from the
// ExternalBoundaryCall disclosure — observability/infrastructure plumbing rather
// than a dependency boundary worth surfacing (the OTel built-in plus any
// static.externalBoundaryExempt prefixes).
func (hs *HintSet) IsExternalBoundaryExempt(fn *ssa.Function) bool {
	return prefixExempt(PkgPath(fn), hs.externalExempt)
}

// IsExternalBoundaryTrivial reports whether fn's package is a known-benign
// (pure-compute / framework) dependency whose ExternalBoundaryCall should be tagged
// SeverityTrivial rather than effect-bearing — the built-in default set plus any
// static.externalBoundaryTrivial prefixes. Unlike IsExternalBoundaryExempt it does
// NOT suppress the disclosure; the caller still emits the blind spot, this only
// chooses its tier. Same segment-boundary prefix match, so the two predicates share
// one matcher (CLAUDE.md: one source of truth).
func (hs *HintSet) IsExternalBoundaryTrivial(fn *ssa.Function) bool {
	return prefixExempt(PkgPath(fn), hs.externalTrivial)
}

// prefixExempt reports whether pkgPath falls under one of the exempt prefixes,
// matched at a path-SEGMENT boundary so "github.com/go-chi/chi/v5" covers
// ".../v5/middleware" but not an unrelated ".../v52". An entry with a trailing
// slash ("go.opentelemetry.io/") is itself the boundary, matching the whole family.
func prefixExempt(pkgPath string, prefixes []string) bool {
	if pkgPath == "" {
		return false
	}
	for _, pre := range prefixes {
		switch {
		case pkgPath == pre:
			return true
		case strings.HasSuffix(pre, "/"):
			if strings.HasPrefix(pkgPath, pre) {
				return true
			}
		case strings.HasPrefix(pkgPath, pre+"/"):
			return true
		}
	}
	return false
}

// StringArgs returns the call's string-typed arguments, in source order. The
// receiver and a leading context are naturally skipped (they are not strings),
// so a publish's event name is StringArgs()[0] and an HTTP seam's (peer, method,
// route) are StringArgs()[0:3].
func StringArgs(site ssa.CallInstruction) []ssa.Value {
	var out []ssa.Value
	for _, a := range site.Common().Args {
		if isStringType(a.Type()) {
			out = append(out, a)
		}
	}
	return out
}

// ConstString returns the Go string value of v and true if v is a constant
// string, else ("", false) — a non-constant boundary argument is not statically
// knowable.
func ConstString(v ssa.Value) (string, bool) {
	c, ok := v.(*ssa.Const)
	if !ok || c.Value == nil || c.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(c.Value), true
}

func isStringType(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Kind() == types.String
}
