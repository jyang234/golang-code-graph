package review

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

const (
	goldensDir = "../../../testdata/groundwork/goldens"
	policyPath = "../../../testdata/groundwork/policies/layeredsvc.json"

	hGetUser     = "(*example.com/layeredsvc/internal/handler.Server).GetUser"
	hGetUserFast = "(*example.com/layeredsvc/internal/handler.Server).GetUserFast"
	aGetProfile  = "(*example.com/layeredsvc/internal/app.Service).GetProfile"
	sSelectUser  = "(*example.com/layeredsvc/internal/store.Store).SelectUser"
)

func loadGraph(t *testing.T, name string) *graph.Graph {
	t.Helper()
	g, err := graph.LoadFile(filepath.Join(goldensDir, name))
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return g
}

func loadPolicy(t *testing.T) *policy.Policy {
	t.Helper()
	p, err := policy.Load(policyPath)
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	return p
}

func TestReviewBlockNamesSkipEdge(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := loadGraph(t, "layeredsvc.branch-skip.graph.json")
	a := Review(p, base, branch)

	if a.Verdict != Block {
		t.Fatalf("verdict = %s, want BLOCK", a.Verdict)
	}
	if len(a.NewViolations) != 1 || a.NewViolations[0].Rule != "layering" {
		t.Fatalf("want one new layering violation, got %v", a.NewViolations)
	}
	if a.NewViolations[0].From != hGetUserFast || a.NewViolations[0].To != sSelectUser {
		t.Errorf("violation edge = %s → %s", a.NewViolations[0].From, a.NewViolations[0].To)
	}
	if a.Shape != CrossPackage {
		t.Errorf("shape = %s, want cross-package", a.Shape)
	}
}

func TestReviewStructurallyClear(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := loadGraph(t, "layeredsvc.branch-good.graph.json")
	a := Review(p, base, branch)

	if a.Verdict != StructurallyClear {
		t.Fatalf("verdict = %s, want STRUCTURALLY-CLEAR (violations: %v)", a.Verdict, a.NewViolations)
	}
	// The new endpoint is reported as an additive entrypoint, not a breaking change.
	if len(a.Contract) != 1 || a.Contract[0].Op != "+" || a.Contract[0].Breaking {
		t.Errorf("contract = %v, want one additive entrypoint", a.Contract)
	}
}

func TestReviewNoStructuralSignal(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	a := Review(p, base, base)
	if a.Verdict != NoStructuralSignal {
		t.Fatalf("identical graphs should abstain; got %s", a.Verdict)
	}
	if a.Shape != BodyOnly {
		t.Errorf("shape = %s, want body-only", a.Shape)
	}
}

// A standing io_budget caution (unenforceable on both base and branch) is
// absent from NewCautions — the delta suppresses it — but must still appear in
// StandingCautions AND survive the NO-STRUCTURAL-SIGNAL abstain render, where a
// body-only change against an unenforceable budget would otherwise hide it (R1).
func TestReviewStandingCautionSurvivesAbstain(t *testing.T) {
	p, g := dbCallPair()
	a := Review(p, g, g) // identical graphs → NO-STRUCTURAL-SIGNAL

	if a.Verdict != NoStructuralSignal {
		t.Fatalf("identical graphs should abstain; got %s", a.Verdict)
	}
	if len(a.NewCautions) != 0 {
		t.Fatalf("a standing caution must not be reported as new; got %v", a.NewCautions)
	}
	if len(a.StandingCautions) == 0 {
		t.Fatal("the artifact must carry the standing io_budget caution")
	}
	if !strings.Contains(a.Render(), "standing caution") {
		t.Errorf("the abstain render must still disclose the standing caution; got:\n%s", a.Render())
	}
}

// The artifact self-certifies the substrate the judged branch was built on, and
// a base/branch substrate mismatch is disclosed as a synthesized caveat — a
// delta computed across substrates can move for the analyzer's reasons, not the
// code's (R3).
func TestReviewRecordsSubstrateAndMismatch(t *testing.T) {
	mk := func(algo string) *graph.Graph {
		return &graph.Graph{
			Algo:    algo,
			Caveats: []string{algo + " note"},
			Nodes:   []graph.Node{{FQN: "(*svc.S).Do", Sig: "func()", Tier: 1}},
		}
	}
	p := &policy.Policy{Service: "svc", Version: 1}

	// Matched substrate: branch algo recorded, no mismatch caveat.
	a := Review(p, mk("rta"), mk("rta"))
	if a.Algo != "rta" {
		t.Fatalf("artifact must record the branch substrate; got %q", a.Algo)
	}
	for _, c := range a.Caveats {
		if strings.Contains(c, "substrate differs") {
			t.Errorf("matched substrate must not synthesize a mismatch caveat; got %q", c)
		}
	}
	if !strings.Contains(a.Render(), "substrate: rta") {
		t.Errorf("render must echo the substrate; got:\n%s", a.Render())
	}

	// Mismatched substrate: branch algo recorded AND a disclosure caveat.
	m := Review(p, mk("rta"), mk("vta"))
	if m.Algo != "vta" {
		t.Fatalf("artifact must record the BRANCH substrate (vta); got %q", m.Algo)
	}
	var disclosed bool
	for _, c := range m.Caveats {
		if strings.Contains(c, "substrate differs") {
			disclosed = true
		}
	}
	if !disclosed {
		t.Errorf("a base/branch substrate mismatch must be disclosed as a caveat; got %v", m.Caveats)
	}
}

// The same feature wired two ways must produce different verdicts from the same
// (absent) prose — the comprehension the reviewer was losing.
func TestSameFeatureDifferentVerdict(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	good := Review(p, base, loadGraph(t, "layeredsvc.branch-good.graph.json"))
	skip := Review(p, base, loadGraph(t, "layeredsvc.branch-skip.graph.json"))
	if good.Verdict == skip.Verdict {
		t.Fatalf("good and skip wirings produced the same verdict %s", good.Verdict)
	}
	if good.Digest == skip.Digest {
		t.Fatal("good and skip artifacts share a digest")
	}
}

func TestReviewReportsOnlyNewViolations(t *testing.T) {
	p := loadPolicy(t)
	// Base already contains the skip edge: it is a pre-existing violation.
	base := loadGraph(t, "layeredsvc.graph.json")
	base.Edges = append(base.Edges, graph.Edge{From: hGetUserFast, To: sSelectUser, Tier: 2})
	base.Nodes = append(base.Nodes, graph.Node{FQN: hGetUserFast, Sig: "func()", Tier: 1})

	// Branch keeps that skip and adds an upward edge (store → handler).
	branch := loadGraph(t, "layeredsvc.graph.json")
	branch.Edges = append(branch.Edges,
		graph.Edge{From: hGetUserFast, To: sSelectUser, Tier: 2},
		graph.Edge{From: sSelectUser, To: hGetUser, Tier: 2})
	branch.Nodes = append(branch.Nodes, graph.Node{FQN: hGetUserFast, Sig: "func()", Tier: 1})

	a := Review(p, base, branch)
	if len(a.NewViolations) != 1 {
		t.Fatalf("want only the newly-introduced upward violation, got %v", a.NewViolations)
	}
	if a.NewViolations[0].Summary != "store → handler calls upward" {
		t.Errorf("new violation = %q, want the upward edge only", a.NewViolations[0].Summary)
	}
}

func TestReviewReachExisting(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	// Add a DB write effect on an existing domain function, reached by the GetUser
	// route — the change is now live behind an existing entrypoint.
	branch := loadGraph(t, "layeredsvc.graph.json")
	branch.Edges = append(branch.Edges, graph.Edge{
		From: aGetProfile, To: "boundary:db INSERT log", Tier: 1, Boundary: "outbound-sync",
	})
	a := Review(p, base, branch)

	if len(a.Reach) != 1 || a.Reach[0] != hGetUser {
		t.Errorf("reach = %v, want [%s]", a.Reach, hGetUser)
	}
	// It also added a write effect, surfaced in the I/O section.
	if len(a.Effects) != 1 || !a.Effects[0].Write {
		t.Errorf("effects = %v, want one write", a.Effects)
	}
}

// A branch that moves a write off constant SQL — the labeler can no longer read
// its verb, so it becomes "db call" — erodes the whole write-surface family at
// once. The review discloses the fidelity drop as a graph-health signal, with no
// policy rule required (F2).
func TestReviewDBLabelDrift(t *testing.T) {
	const route = "(*example.com/svc/internal/handler.Server).Create"
	const store = "(*example.com/svc/internal/store.Store).Insert"
	mk := func(dbLabel string) *graph.Graph {
		return &graph.Graph{
			Nodes: []graph.Node{
				{FQN: route, Sig: "func()", Tier: 1},
				{FQN: store, Sig: "func() error", Tier: 1},
			},
			Edges: []graph.Edge{
				{From: route, To: store, Tier: 2},
				{From: store, To: "boundary:db " + dbLabel, Tier: 1, Boundary: "outbound-sync"},
			},
		}
	}
	base := mk("INSERT things") // classified
	branch := mk("call")        // moved off constant SQL — now unreadable
	p := &policy.Policy{Service: "svc", Version: 1}

	a := Review(p, base, branch)
	if a.DBLabelDrift == nil {
		t.Fatal("a classified→unclassified DB move must surface label drift")
	}
	if a.DBLabelDrift.Base != 0 || a.DBLabelDrift.Branch != 1 {
		t.Fatalf("drift = %+v, want base 0 → branch 1", *a.DBLabelDrift)
	}
	// The reverse move (gaining fidelity) is not a regression — stay silent.
	if rev := Review(p, branch, base); rev.DBLabelDrift != nil {
		t.Fatalf("regaining fidelity must not report drift; got %+v", *rev.DBLabelDrift)
	}
}

func TestReviewDeterministic(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := loadGraph(t, "layeredsvc.branch-skip.graph.json")
	if a, b := Review(p, base, branch), Review(p, base, branch); a.Digest != b.Digest {
		t.Fatalf("non-deterministic digest: %s vs %s", a.Digest, b.Digest)
	}
}

func TestVerifyAuthentic(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := loadGraph(t, "layeredsvc.branch-skip.graph.json")
	a := Review(p, base, branch)
	if res := VerifyArtifact(a, p, base, branch); !res.OK() {
		t.Fatalf("authentic artifact failed verification: %s — %s", res.Status, res.Detail)
	}
}

func TestVerifyTampered(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := loadGraph(t, "layeredsvc.branch-skip.graph.json")
	a := Review(p, base, branch)
	a.Verdict = StructurallyClear // edit a field, leave the digest
	if res := VerifyArtifact(a, p, base, branch); res.Status != Tampered {
		t.Fatalf("status = %s, want TAMPERED", res.Status)
	}
}

func TestVerifyStaleWrongCode(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	a := Review(p, base, loadGraph(t, "layeredsvc.branch-skip.graph.json"))
	// Verify the skip artifact against the GOOD branch — different code.
	if res := VerifyArtifact(a, p, base, loadGraph(t, "layeredsvc.branch-good.graph.json")); res.Status != Stale {
		t.Fatalf("status = %s, want STALE", res.Status)
	}
}

// The sharpest case from the pressure test: an agent edits the body AND recomputes
// the digest over the lie. Body integrity passes; the recomputation from the
// trusted graphs still catches it.
func TestVerifyResignedForgeryIsStale(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := loadGraph(t, "layeredsvc.branch-skip.graph.json")
	a := Review(p, base, branch)

	a.Verdict = StructurallyClear
	a.NewViolations = nil
	a.Digest = digestOf(a) // re-sign over the doctored body

	res := VerifyArtifact(a, p, base, branch)
	if res.Status != Stale {
		t.Fatalf("re-signed forgery status = %s, want STALE (caught by recomputation)", res.Status)
	}
}
