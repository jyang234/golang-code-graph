package reclaim_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/callgraph"
	"github.com/jyang234/golang-code-graph/internal/static/frontier"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
	"github.com/jyang234/golang-code-graph/internal/static/reclaim"
)

func analyzeFixture(t *testing.T, name string) *analyze.Result {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", name)
	res, err := analyze.Analyze(dir, callgraph.Options{Algo: callgraph.AlgoVTA})
	if err != nil {
		t.Fatalf("analyze %s: %v", name, err)
	}
	return res
}

// On the strict-server fixture the reclaimer recovers exactly the three
// wrapper→closure edges the builder lost at the http.Handler dispatch — one per
// operation — each attributed to the strict-server reclaimer. The From is always
// the wrapper method (no `$`), the To its own `$1` handler closure.
func TestStrictServerReclaimsTheSeam(t *testing.T) {
	edges := reclaim.StrictServer(analyzeFixture(t, "strictsvc"))
	if len(edges) != 3 {
		t.Fatalf("want 3 recovered seam edges, got %d: %+v", len(edges), edges)
	}
	got := map[string]bool{}
	for _, e := range edges {
		if e.Via != reclaim.ViaStrictServer {
			t.Errorf("edge %v not attributed to the reclaimer (via=%q)", e, e.Via)
		}
		if strings.Contains(e.From, "$") {
			t.Errorf("From should be the wrapper method, not a closure: %q", e.From)
		}
		if !strings.HasSuffix(e.To, "$1") {
			t.Errorf("To should be the per-handler $1 closure: %q", e.To)
		}
		// The edge connects a method to its OWN closure (same FQN prefix).
		if base := strings.TrimSuffix(e.To, "$1"); base != e.From {
			t.Errorf("edge does not connect a method to its own closure: %s -> %s", e.From, e.To)
		}
		got[e.From] = true
	}
	for _, op := range []string{"CreateEventTypeTemplate", "SyncEventTypes", "GetHealth"} {
		want := "api.ServerInterfaceWrapper)." + op
		found := false
		for from := range got {
			if strings.HasSuffix(from, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("missing recovered edge for %s; got %v", op, got)
		}
	}
}

// Soundness / no false positives: the reclaimer fires ONLY at a ServeHTTP dispatch
// seam. A non-strict service (wrapper methods registered directly, no closure, no
// ServeHTTP) yields nothing; and loansvc — which HAS severed `$N` closures, but
// goroutine/callback ones, not ServeHTTP dispatch — is left untouched. A reclaimer
// that fired on "any anonymous closure its parent contains" would wrongly connect
// those callbacks (the parent PASSES them, never CALLS them).
func TestStrictServerNoFalsePositives(t *testing.T) {
	for _, name := range []string{"oapisvc", "loansvc"} {
		if edges := reclaim.StrictServer(analyzeFixture(t, name)); len(edges) != 0 {
			t.Errorf("%s has no strict-server ServeHTTP seam; want 0 recovered edges, got %+v", name, edges)
		}
	}
}

// TestStrictServerR2DoesNotConnectUnservedClosures is probe #3 of the audit: the
// reclaimer's R2 boundary — it must add ONLY edges real execution can take, never a
// closure the method merely passes. The existing no-false-positive test uses services
// with no ServeHTTP dispatch at all; this one puts a confounder INSIDE a dispatching
// method, where a loosened flowsTo fails.
//
// reclaimsvc.wrapper.Admin dispatches via handler.ServeHTTP but holds TWO closures:
// the SERVED one (Admin$1, which reaches the receiver through the middleware Phi — so
// this also exercises flowsTo's Phi branch) and a SIBLING (Admin$2) the method only
// passes to runLater, never serving it. A reclaimer that connected "any AnonFunc in a
// method that calls ServeHTTP" would also emit Admin→Admin$2; the flow requirement
// must gate it to exactly 1 — Admin→Admin$1 — because only that closure provably flows
// to a ServeHTTP receiver. Connecting Admin$2 is an edge execution cannot take via the
// dispatch, an R2 violation that could later launder a false coverage/effect-
// attribution proof (the false-positive-edge pole this probe guards). A mutation that
// drops the flow requirement is confirmed to make this test emit the spurious Admin$2
// edge.
//
// authMW supplies the middleware Phi and can SHORT-CIRCUIT (no Authorization → 401, the
// handler is never called). The reclaimed Admin→Admin$1 edge is therefore a MAY edge —
// execution CAN take it on the authorized path, which is precisely R2's "can take"
// requirement; a spurious edge would only ever over-block a negative check, never
// manufacture a false provenAbsent.
func TestStrictServerR2DoesNotConnectUnservedClosures(t *testing.T) {
	edges := reclaim.StrictServer(analyzeFixture(t, "reclaimsvc"))
	if len(edges) != 1 {
		t.Fatalf("R2: want exactly 1 reclaimed edge (the served closure), got %d: %+v", len(edges), edges)
	}
	e := edges[0]
	if e.Via != reclaim.ViaStrictServer {
		t.Errorf("edge not attributed to the reclaimer: via=%q", e.Via)
	}
	if !strings.HasSuffix(e.From, "wrapper).Admin") {
		t.Errorf("From should be the dispatching method wrapper.Admin, got %q", e.From)
	}
	if !strings.HasSuffix(e.To, "wrapper).Admin$1") {
		t.Errorf("To should be the SERVED closure Admin$1, got %q", e.To)
	}
	// The sibling closure the method merely passes must never be a reclaim target.
	if strings.Contains(e.To, "Admin$2") {
		t.Errorf("R2 violation: connected the unserved sibling closure %q via %q", e.To, e.From)
	}
}

// Integration: folding the reclaimed edges into the graph closes the seam — every
// route reaches its effects, so the frontier reports zero attribution loss and no
// B (severed-closure / starved-entrypoint) markers, leaving only the genuinely
// irreducible A/B2 frontier. This is the win the strictsvc characterization test
// (boundary package, no reclaim) is written to flip to.
func TestApplyReclaimersClosesSeam(t *testing.T) {
	res := analyzeFixture(t, "strictsvc")
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if added := graphio.ApplyReclaimers(g, res); added != 3 {
		t.Fatalf("want 3 edges folded in, got %d", added)
	}

	taggedFound := false
	for _, e := range g.Edges {
		if e.Via == reclaim.ViaStrictServer {
			taggedFound = true
		}
	}
	if !taggedFound {
		t.Error("reclaimed edges must carry their Via provenance in the graph")
	}

	r := frontier.Summarize(graphio.ClassifyFrontier(g), len(g.Entrypoints))
	if r.StarvedEntrypoints != 0 || r.AttributionLoss != 0 {
		t.Errorf("seam not closed: %d/%d starved (%.2f), want 0", r.StarvedEntrypoints, r.Entrypoints, r.AttributionLoss)
	}
	if r.Counts[frontier.BinB] != 0 {
		t.Errorf("reclaim must clear the B frontier; got B=%d markers=%+v", r.Counts[frontier.BinB], r.Markers)
	}
	// The reclaimed routes now reach their effects, so they are neither severed nor
	// unconfirmed.
	if len(r.UnconfirmedRoutes) != 0 {
		t.Errorf("reclaim must leave no unconfirmed routes, got %v", r.UnconfirmedRoutes)
	}
	if g.Frontier != nil {
		for _, m := range g.Frontier.Markers {
			if m.Kind == "severed-closure" || m.Kind == "starved-entrypoint" {
				t.Errorf("reclaimed graph must carry no seam marker, got %+v", m)
			}
		}
	}
}
