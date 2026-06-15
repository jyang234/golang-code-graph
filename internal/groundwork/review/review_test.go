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
	// branch-good adds GetUserFast — a new exported handler METHOD that is not
	// bound to any route (the entrypoints join is unchanged). It is a graph root
	// but not an external entrypoint, so it is internal structure, not a contract
	// change: the contract section stays empty and the new method surfaces in the
	// structural delta instead (§9 — roots are not the contract).
	if len(a.Contract) != 0 {
		t.Errorf("contract = %v, want no external-contract change for an unwired exported method", a.Contract)
	}
	if a.Shape != CrossPackage {
		t.Errorf("the new method must still register as a structural change; shape = %s", a.Shape)
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

// Internal identity churn — a graph root that is not an external entrypoint —
// must not read as a breaking external-contract change: a closure renumbered by a
// refactor and an internal function left rootless by a deleted backend are both
// roots but neither is the contract. A removed HTTP route (a real external
// entrypoint) still blocks (§9).
func TestContractDistinguishesInternalRootChurnFromExternalRemoval(t *testing.T) {
	const (
		routeFn  = "(*svc/internal/handler.Server).GetUser"
		store    = "(*svc/internal/store.Store).Select"
		internal = "svc/internal/worker.pollMessages"
		closure  = "svc.newHTTPServer$1"
	)
	base := &graph.Graph{
		Algo: "vta",
		Nodes: []graph.Node{
			{FQN: routeFn, Sig: "f", Tier: 1}, {FQN: store, Sig: "f", Tier: 2},
			{FQN: internal, Sig: "f", Tier: 1}, {FQN: closure, Sig: "f", Tier: 1},
		},
		Edges: []graph.Edge{
			{From: routeFn, To: store}, {From: internal, To: store}, {From: closure, To: store},
		},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "GET /users/{id}", Fn: routeFn}},
	}
	p := &policy.Policy{Service: "svc", Version: 1}

	// Branch drops the internal root and the closure (their call sites removed):
	// both become absent roots, neither is an entrypoint → no breaking contract.
	internalChurn := &graph.Graph{
		Algo:        "vta",
		Nodes:       []graph.Node{{FQN: routeFn, Sig: "f", Tier: 1}, {FQN: store, Sig: "f", Tier: 2}},
		Edges:       []graph.Edge{{From: routeFn, To: store}},
		Entrypoints: base.Entrypoints,
	}
	a := Review(p, base, internalChurn)
	for _, c := range a.Contract {
		if c.Breaking {
			t.Errorf("internal root/closure removal must not be a breaking contract change; got %+v", c)
		}
	}
	if anyBreaking(a.Contract) {
		t.Error("verdict must not be driven BREAKING by internal identity churn")
	}

	// Branch drops the HTTP route's handler — a real external entrypoint removal.
	routeRemoved := &graph.Graph{
		Algo: "vta",
		Nodes: []graph.Node{
			{FQN: store, Sig: "f", Tier: 2}, {FQN: internal, Sig: "f", Tier: 1}, {FQN: closure, Sig: "f", Tier: 1},
		},
		Edges:       []graph.Edge{{From: internal, To: store}, {From: closure, To: store}},
		Entrypoints: base.Entrypoints,
	}
	b := Review(p, base, routeRemoved)
	var sawBreakingRoute bool
	for _, c := range b.Contract {
		if c.Op == "-" && c.Surface == "entrypoint" && c.Breaking {
			sawBreakingRoute = true
		}
	}
	if !sawBreakingRoute {
		t.Errorf("a removed HTTP route must be a breaking entrypoint contract change; got %+v", b.Contract)
	}
}

// A policy proposed on one algorithm but gated against a branch graph built on
// another must disclose the mismatch on the substrate line, so its potentially
// spurious reachability findings are read as analyzer artifacts (§9). Silent when
// the policy's substrate matches the branch's, or is unrecorded.
func TestReviewFlagsPolicyGraphSubstrateMismatch(t *testing.T) {
	mk := func() *graph.Graph {
		return &graph.Graph{Algo: "rta", Nodes: []graph.Node{{FQN: "(*svc.S).Do", Sig: "func()", Tier: 1}}}
	}
	hasMismatch := func(cs []string) bool {
		for _, c := range cs {
			if strings.Contains(c, "substrate mismatch") {
				return true
			}
		}
		return false
	}

	a := Review(&policy.Policy{Service: "svc", Version: 1, Substrate: "vta"}, mk(), mk())
	if !hasMismatch(a.Caveats) {
		t.Errorf("a vta policy gated on an rta graph must disclose a substrate mismatch; got %v", a.Caveats)
	}
	if !strings.Contains(a.Render(), "substrate mismatch") {
		t.Errorf("render must echo the mismatch; got:\n%s", a.Render())
	}

	if m := Review(&policy.Policy{Service: "svc", Version: 1, Substrate: "rta"}, mk(), mk()); hasMismatch(m.Caveats) {
		t.Errorf("a matching substrate must not synthesize a mismatch caveat; got %v", m.Caveats)
	}
	if u := Review(&policy.Policy{Service: "svc", Version: 1}, mk(), mk()); hasMismatch(u.Caveats) {
		t.Errorf("an unrecorded policy substrate must not synthesize a mismatch caveat; got %v", u.Caveats)
	}
}

// The substrate mismatch must be a DISCLOSURE only — it must never enter the
// base-vs-branch finding diff. A body-only MR (identical structure) judged across
// a base/branch substrate switch must still abstain NO-STRUCTURAL-SIGNAL: the
// mismatch is a build artifact, not signal about the change. (Regression: emitting
// it as a fitness.Check Caution leaked it into NewCautions and flipped the verdict
// to STRUCTURALLY-CLEAR.)
func TestSubstrateMismatchDoesNotFlipBodyOnlyVerdict(t *testing.T) {
	nodes := []graph.Node{{FQN: "(*svc.S).Do", Sig: "func()", Tier: 1}}
	base := &graph.Graph{Algo: "vta", Nodes: nodes}
	branch := &graph.Graph{Algo: "rta", Nodes: nodes} // identical structure, different algo
	a := Review(&policy.Policy{Service: "svc", Version: 1, Substrate: "vta"}, base, branch)

	if a.Verdict != NoStructuralSignal {
		t.Errorf("a body-only change across a substrate switch must abstain; got %s (cautions=%v)", a.Verdict, a.NewCautions)
	}
	for _, c := range a.NewCautions {
		if strings.Contains(c.Summary, "substrate") {
			t.Errorf("the substrate mismatch must not appear as a new caution; got %+v", c)
		}
	}
	// The disclosure itself is preserved — on the caveat line, not as a finding.
	for _, c := range a.Caveats {
		if strings.Contains(c, "substrate mismatch") {
			return
		}
	}
	t.Error("the substrate mismatch must still be disclosed as a caveat")
}

// A branch graph built with `--reclaim` was judged over a substrate that includes
// edges recovered at a dispatch seam; the verdict must disclose it on the substrate
// line so a reclaim-informed gate is auditable, not silently folded into a plain
// pass (R9).
func TestReviewDisclosesReclaimedSubstrate(t *testing.T) {
	mk := func(reclaimed bool) *graph.Graph {
		e := graph.Edge{From: "(*svc.S).Do", To: "(*svc.S).Do$1"}
		if reclaimed {
			e.Via = "strict-server"
		}
		return &graph.Graph{
			Algo:  "vta",
			Nodes: []graph.Node{{FQN: "(*svc.S).Do", Sig: "func()", Tier: 1}, {FQN: "(*svc.S).Do$1", Sig: "func()", Tier: 1}},
			Edges: []graph.Edge{e},
		}
	}
	p := &policy.Policy{Service: "svc", Version: 1}

	a := Review(p, mk(true), mk(true))
	var disclosed bool
	for _, c := range a.Caveats {
		if strings.Contains(c, "reclaim-informed") {
			disclosed = true
		}
	}
	if !disclosed {
		t.Errorf("a reclaimed branch substrate must be disclosed as a caveat; got %v", a.Caveats)
	}
	if !strings.Contains(a.Render(), "reclaim-informed") {
		t.Errorf("render must echo the reclaim disclosure; got:\n%s", a.Render())
	}

	// A base (no --reclaim) branch discloses nothing.
	b := Review(p, mk(false), mk(false))
	for _, c := range b.Caveats {
		if strings.Contains(c, "reclaim-informed") {
			t.Errorf("a base branch substrate must not synthesize a reclaim caveat; got %q", c)
		}
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
