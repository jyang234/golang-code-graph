package features

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/config"
)

// TestBuiltinTelemetryIncludesZap pins zap as a built-in telemetry library: a
// service should not need a per-service telemetry hint just to keep its zap
// logging calls out of the tier-1..3 bands. The check is white-box because the
// fixture does not import zap, so there is no zap *ssa.Function to classify
// end-to-end — the guarantee is that the hint is registered by default.
func TestBuiltinTelemetryIncludesZap(t *testing.T) {
	hs := NewHintSet(nil) // nil cfg => only built-ins
	want := map[string]bool{
		"go.uber.org/zap":         false,
		"go.uber.org/zap/zapcore": false,
	}
	for _, h := range hs.telemetry {
		if _, ok := want[h.pkgPath]; ok && h.name == "" { // a bare-path hint matches any call into the package
			want[h.pkgPath] = true
		}
	}
	for pkg, found := range want {
		if !found {
			t.Errorf("built-in telemetry hints missing a bare-path entry for %q", pkg)
		}
	}
}

// TestBuiltinExternalBoundaryExemptsOTel pins OpenTelemetry as a built-in exempt
// prefix: even with nil config its span/attribute packages must not surface as an
// ExternalBoundaryCall, or every instrumented function would disclose one.
func TestBuiltinExternalBoundaryExemptsOTel(t *testing.T) {
	hs := NewHintSet(nil) // nil cfg => only built-ins
	for _, p := range []string{"go.opentelemetry.io/otel", "go.opentelemetry.io/otel/trace", "go.opentelemetry.io/otel/attribute"} {
		if !prefixExempt(p, hs.externalExempt) {
			t.Errorf("OpenTelemetry package %q should be exempt by default", p)
		}
	}
}

// TestExternalBoundaryTrivialSet pins the §21.A tier classifier: the named
// framework/utility packages (uuid, chi, oapi-codegen runtime) are trivial by
// default, an unrecognized dependency is NOT (it defaults to effect-bearing — disclose,
// don't pre-judge), and config extends the set. errgroup is deliberately absent from
// the built-ins (it orchestrates effect-bearing closures), so it is trivial only when
// declared. IsExternalBoundaryTrivial delegates to prefixExempt over externalTrivial,
// so testing the field is the faithful unit (the SSA-driven path is covered in
// blindspots).
func TestExternalBoundaryTrivialSet(t *testing.T) {
	hs := NewHintSet(nil) // nil cfg => only built-ins
	for _, p := range []string{"github.com/google/uuid", "github.com/go-chi/chi/v5", "github.com/go-chi/chi/v5/middleware", "github.com/oapi-codegen/runtime"} {
		if !prefixExempt(p, hs.externalTrivial) {
			t.Errorf("%q should be a built-in trivial prefix", p)
		}
	}
	for _, p := range []string{"golang.org/x/sync/errgroup", "github.com/customerio/go-customerio", "github.com/aws/aws-sdk-go-v2/service/sns"} {
		if prefixExempt(p, hs.externalTrivial) {
			t.Errorf("%q must NOT be trivial by default (effect-bearing — disclose)", p)
		}
	}
	// Config extends the set without disturbing the built-ins.
	withCfg := NewHintSet(&config.Config{Static: config.StaticConfig{ExternalBoundaryTrivial: []string{"golang.org/x/sync"}}})
	if !prefixExempt("golang.org/x/sync/errgroup", withCfg.externalTrivial) {
		t.Error("config externalBoundaryTrivial should extend the trivial set")
	}
	if !prefixExempt("github.com/google/uuid", withCfg.externalTrivial) {
		t.Error("config must not drop the built-in trivial prefixes")
	}
}

// TestPrefixExempt pins the segment-boundary matching the externalBoundaryExempt
// list relies on: a bare entry matches itself and its subpackages but not a
// look-alike sibling; a trailing-slash entry matches the whole family.
func TestPrefixExempt(t *testing.T) {
	prefixes := []string{"github.com/go-chi/chi/v5", "go.opentelemetry.io/"}
	cases := []struct {
		path string
		want bool
	}{
		{"github.com/go-chi/chi/v5", true},            // exact
		{"github.com/go-chi/chi/v5/middleware", true}, // subpackage at a segment boundary
		{"github.com/go-chi/chi/v52", false},          // look-alike sibling, not a boundary
		{"github.com/go-chi/chi", false},              // parent is not under the prefix
		{"go.opentelemetry.io/otel", true},            // trailing-slash family
		{"go.opentelemetry.io/otel/trace", true},
		{"golang.org/x/sync/errgroup", false}, // unrelated
		{"", false},                           // synthetic / no package
	}
	for _, c := range cases {
		if got := prefixExempt(c.path, prefixes); got != c.want {
			t.Errorf("prefixExempt(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
