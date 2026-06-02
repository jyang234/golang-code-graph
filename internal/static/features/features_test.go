package features_test

import (
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/model"
	"github.com/jyang234/golang-code-graph/internal/static/features"
	"github.com/jyang234/golang-code-graph/internal/static/ssabuild"
	"github.com/jyang234/golang-code-graph/internal/static/statictest"
)

func setup(t *testing.T) (*features.Extractor, *ssabuild.Program) {
	t.Helper()
	res, err := statictest.Analyze()
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	return features.NewExtractor(res.Config, res.Program.ModulePath), res.Program
}

// callTo finds the first static call from fn to a callee whose name contains
// calleeSubstr, returning the callee and the call site.
func callTo(fn *ssa.Function, calleeSubstr string) (*ssa.Function, ssa.CallInstruction) {
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			call, ok := instr.(ssa.CallInstruction)
			if !ok {
				continue
			}
			if c := call.Common().StaticCallee(); c != nil && strings.Contains(c.RelString(nil), calleeSubstr) {
				return c, call
			}
		}
	}
	return nil, nil
}

func TestHintPredicates(t *testing.T) {
	ext, prog := setup(t)
	h := ext.Hints()
	cases := []struct {
		fqn  string
		pred func(*ssa.Function) bool
		name string
	}{
		{"(*example.com/loansvc/internal/eventbus.Bus).Publish", h.IsPublish, "IsPublish"},
		{"(*example.com/loansvc/internal/eventbus.Bus).Subscribe", h.IsConsume, "IsConsume"},
		{"(*example.com/loansvc/internal/client.Client).Call", h.IsHTTP, "IsHTTP"},
		{"(*database/sql.DB).QueryRowContext", h.IsDB, "IsDB"},
	}
	for _, tc := range cases {
		fn := statictest.FindFuncExact(prog, tc.fqn)
		if fn == nil {
			t.Errorf("%s: function %q not found", tc.name, tc.fqn)
			continue
		}
		if !tc.pred(fn) {
			t.Errorf("%s(%s) = false, want true", tc.name, fn.RelString(nil))
		}
	}
}

func TestPureClassification(t *testing.T) {
	ext, _ := setup(t)
	if tier, _ := ext.Classify(ext.Inbound("POST /x", false)); tier != 1 {
		t.Errorf("inbound tier = %d, want 1", tier)
	}
	if tier, _ := ext.Classify(ext.Published("loan.approved")); tier != 1 {
		t.Errorf("published tier = %d, want 1", tier)
	}
	if tier, _ := ext.Classify(ext.External("credit-bureau GET /x")); tier != 1 {
		t.Errorf("external tier = %d, want 1", tier)
	}
}

func TestDBEffects(t *testing.T) {
	ext, prog := setup(t)

	read := statictest.FindFunc(prog, "store.Loans).SelectApplicant")
	callee, site := callTo(read, "QueryRow")
	if callee == nil {
		t.Fatal("no QueryRow call in SelectApplicant")
	}
	f := ext.Edge(read, callee, site)
	if f.Boundary != model.BoundaryOutboundSync || f.Effect != model.EffectRead {
		t.Errorf("DB read features = %+v, want outbound-sync/read", f)
	}
	if tier, _ := ext.Classify(f); tier != 2 {
		t.Errorf("DB read tier = %d, want 2 (ext-read)", tier)
	}

	mutate := statictest.FindFunc(prog, "store.Loans).InsertLedger")
	callee, site = callTo(mutate, "Exec")
	if callee == nil {
		t.Fatal("no Exec call in InsertLedger")
	}
	f = ext.Edge(mutate, callee, site)
	if f.Effect != model.EffectMutate {
		t.Errorf("DB write effect = %q, want mutate", f.Effect)
	}
	if tier, _ := ext.Classify(f); tier != 1 {
		t.Errorf("DB write tier = %d, want 1 (mutate)", tier)
	}
}

// TestConsumeSeamTiers proves the receive side of the bus is classified as an
// inbound boundary (tier 1), symmetric to publish — not left as compute, where
// the consume seam would be invisible.
func TestConsumeSeamTiers(t *testing.T) {
	ext, prog := setup(t)
	run := statictest.FindFunc(prog, "loansvc.run")
	if run == nil {
		t.Fatal("run not found")
	}
	callee, site := callTo(run, "Bus).Subscribe")
	if callee == nil {
		t.Fatal("no Subscribe call in run")
	}
	f := ext.Edge(run, callee, site)
	if f.Boundary != model.BoundaryInbound {
		t.Errorf("consume boundary = %q, want inbound", f.Boundary)
	}
	if tier, _ := ext.Classify(f); tier != 1 {
		t.Errorf("consume tier = %d, want 1 (symmetric to publish)", tier)
	}
}

// TestResultCursorIsNotDBBoundary proves a result-decoding method (Row.Scan)
// is not treated as a DB boundary call: the round-trip already happened in the
// QueryRow* call, so Scan must not surface as a second, tier-1 DB edge.
func TestResultCursorIsNotDBBoundary(t *testing.T) {
	ext, prog := setup(t)
	h := ext.Hints()
	read := statictest.FindFunc(prog, "store.Loans).SelectApplicant")
	scan, _ := callTo(read, "Row).Scan")
	if scan == nil {
		t.Fatal("no Row.Scan call in SelectApplicant")
	}
	if h.IsDB(scan) {
		t.Errorf("%s should not be a DB boundary (it decodes an already-fetched row)", scan.RelString(nil))
	}
	// The query that actually hits the database still is a DB boundary.
	q, _ := callTo(read, "QueryRow")
	if q == nil || !h.IsDB(q) {
		t.Error("QueryRow* should be a DB boundary")
	}
}

func TestExternalCallIsIONotRead(t *testing.T) {
	ext, prog := setup(t)
	// A GET to a peer service must be effect=io (tier 1 ext-sync), NOT effect=read
	// (which is reserved for DB reads, tier 2).
	score := statictest.FindFunc(prog, "client.Bureau).Score")
	callee, site := callTo(score, "client.Client).Call")
	if callee == nil {
		t.Fatal("no Client.Call in Bureau.Score")
	}
	f := ext.Edge(score, callee, site)
	if f.Boundary != model.BoundaryOutboundSync || f.Effect != model.EffectIO {
		t.Errorf("external GET features = %+v, want outbound-sync/io", f)
	}
	if tier, _ := ext.Classify(f); tier != 1 {
		t.Errorf("external GET tier = %d, want 1", tier)
	}
}

func TestConcurrentAndOrigin(t *testing.T) {
	ext, prog := setup(t)

	// The fire-and-forget `go e.auditLog(...)` is a concurrent dispatch.
	disburse := statictest.FindFunc(prog, "origination.Evaluator).disburse")
	callee, site := callTo(disburse, "Evaluator).auditLog")
	if callee == nil {
		t.Fatal("no auditLog call in disburse")
	}
	if f := ext.Edge(disburse, callee, site); !f.Concurrent {
		t.Errorf("go auditLog should be concurrent: %+v", f)
	}

	// errgroup is a third-party dependency. Use an exact match so we get Evaluate
	// itself, not one of its closures (Evaluate$1, Evaluate$2).
	eval := statictest.FindFuncExact(prog, "(*example.com/loansvc/internal/origination.Evaluator).Evaluate")
	if eval == nil {
		t.Fatal("Evaluate not found")
	}
	callee, site = callTo(eval, "errgroup")
	if callee == nil {
		t.Fatal("no errgroup call in Evaluate")
	}
	if f := ext.Edge(eval, callee, site); f.Origin != model.OriginThirdParty {
		t.Errorf("errgroup origin = %q, want third-party", f.Origin)
	}
}
