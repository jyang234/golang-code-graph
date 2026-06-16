package features_test

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/features"
	"github.com/jyang234/golang-code-graph/internal/static/statictest"
)

// PkgPath is the single source of truth for package attribution (blindspots and
// obligations delegate to it). It must be nil-safe: obligations relies on
// PkgPath(nil) == "" before applying its own Object() fallback, so a panic here
// would crash that path instead of degrading to "".
func TestPkgPath(t *testing.T) {
	if got := features.PkgPath(nil); got != "" {
		t.Errorf("PkgPath(nil) = %q, want \"\"", got)
	}

	res, err := statictest.Analyze()
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	eval := statictest.FindFuncExact(res.Program, "(*example.com/loansvc/internal/origination.Evaluator).Evaluate")
	if eval == nil {
		t.Fatal("Evaluate not found")
	}
	if got := features.PkgPath(eval); got != "example.com/loansvc/internal/origination" {
		t.Errorf("PkgPath(Evaluate) = %q, want the defining package path", got)
	}
}
