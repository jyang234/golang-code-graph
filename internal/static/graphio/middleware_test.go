package graphio_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
	"github.com/jyang234/golang-code-graph/internal/static/callgraph"
	"github.com/jyang234/golang-code-graph/internal/static/frontier"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
	"github.com/jyang234/golang-code-graph/internal/static/reclaim"
)

func analyzeFixtureVTA(t *testing.T, name string) *analyze.Result {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", name)
	res, err := analyze.Analyze(dir, callgraph.Options{Algo: callgraph.AlgoVTA})
	if err != nil {
		t.Fatalf("analyze %s: %v", name, err)
	}
	return res
}

// reaches reports whether the forward reach of fromSuffix in g crosses any node whose FQN
// ends with toSuffix.
func reaches(g *graphio.Graph, fromSuffix, toSuffix string) bool {
	var from string
	for _, n := range g.Nodes {
		if strings.HasSuffix(n.FQN, fromSuffix) {
			from = n.FQN
		}
	}
	if from == "" {
		return false
	}
	reachable := map[string]bool{from: true}
	for changed := true; changed; {
		changed = false
		for _, e := range g.Edges {
			if reachable[e.From] && !reachable[e.To] {
				reachable[e.To] = true
				changed = true
			}
		}
	}
	for to := range reachable {
		if strings.HasSuffix(to, toSuffix) {
			return true
		}
	}
	return false
}

func unresolvedAt(g *graphio.Graph, siteSuffix string) bool {
	for _, b := range g.BlindSpots {
		if b.Kind == blindspots.UnresolvedCall && strings.HasSuffix(b.Site, siteSuffix) {
			return true
		}
	}
	return false
}

// Integration (the issue's acceptance): on the empty-middleware reproducer shape, applying
// the middleware reclaimer closes the seam — the UnresolvedCall at EmptyWrapper.apply is
// dropped, the read route reaches only its read-only work, and the write route now reaches
// dbWrite (a determinate violation rather than a blind abstention). Build never folds these,
// so the default graph is unchanged (precondition checks below).
func TestApplyMiddlewareReclaimerClosesEmptySeam(t *testing.T) {
	res := analyzeFixtureVTA(t, "mwchainsvc")
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Precondition: the seam is open and the write route is invisible before reclaim.
	if !unresolvedAt(g, "EmptyWrapper).apply") {
		t.Fatalf("precondition: the empty middleware loop should be an UnresolvedCall before reclaim")
	}
	if reaches(g, "EmptyWrapper).PostItems", "mwchainsvc.dbWrite") {
		t.Fatalf("precondition: the write route should be invisible (severed) before reclaim")
	}

	added, cleared := graphio.ApplyMiddlewareReclaimer(g, res)
	if added == 0 {
		t.Fatal("ApplyMiddlewareReclaimer folded no edges")
	}
	// Exactly the two provably-empty loops clear (the factored EmptyWrapper.apply and the
	// inline InlineWrapper.Route); the dynamic / escaping / sibling loops are left blind.
	if cleared != 2 {
		t.Fatalf("want exactly 2 cleared seams (the empty factored + inline loops), got %d", cleared)
	}

	taggedFound := false
	for _, e := range g.Edges {
		if e.Via == reclaim.ViaMiddlewareChain {
			taggedFound = true
		}
	}
	if !taggedFound {
		t.Error("recovered edges must carry their via=middleware-chain provenance in the graph")
	}

	if unresolvedAt(g, "EmptyWrapper).apply") {
		t.Error("after --reclaim-middleware the empty middleware seam must be cleared")
	}
	if !reaches(g, "EmptyWrapper).PostItems", "mwchainsvc.dbWrite") {
		t.Error("after --reclaim-middleware the write route must reach dbWrite (the determinate violation)")
	}
	if reaches(g, "EmptyWrapper).GetItems", "mwchainsvc.dbWrite") {
		t.Error("the read route must not reach dbWrite — it is genuinely read-only")
	}
}

// Soundness (acceptance criterion 3): the dynamic wrapper's seam survives folding. Its
// middleware comes from an opaque package var the reclaimer cannot enumerate, so the loop is
// left blind — never cleared — and no via=middleware-chain edge is folded for it. A false
// edge or a false clear here would be a false PROVEN.
func TestApplyMiddlewareReclaimerLeavesDynamicSeam(t *testing.T) {
	res := analyzeFixtureVTA(t, "mwchainsvc")
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !unresolvedAt(g, "DynWrapper).apply") {
		t.Fatalf("precondition: the dynamic middleware loop should be blind before reclaim")
	}
	graphio.ApplyMiddlewareReclaimer(g, res)

	if !unresolvedAt(g, "DynWrapper).apply") {
		t.Error("an unprovable (dynamic) middleware set must stay blind, not be cleared")
	}
	for _, e := range g.Edges {
		if e.Via == reclaim.ViaMiddlewareChain && strings.Contains(e.From, "DynWrapper") {
			t.Errorf("the dynamic loop must recover no edge; got %s -> %s", e.From, e.To)
		}
	}
}

// The DEFAULT frontier (no flags) predicts middleware reclaimability: a provably-empty loop's
// standalone UnresolvedCall is binned B (reclaimable by --reclaim-middleware), while a
// dynamic / escaping / sibling-return loop stays A (irreducible). This is the guidance the
// frontier exists to give, derived from the same dry run ApplyMiddlewareReclaimer folds.
func TestFrontierPredictsMiddlewareReclaimable(t *testing.T) {
	res := analyzeFixtureVTA(t, "mwchainsvc")
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	r := graphio.ClassifyFrontier(g)
	bin := map[string]string{} // site-suffix -> bin, for the middleware UnresolvedCall markers
	for _, m := range r.Markers {
		if m.Kind != string(blindspots.UnresolvedCall) {
			continue
		}
		switch {
		case strings.HasSuffix(m.Site, "EmptyWrapper).apply"):
			bin["empty"] = string(m.Bin)
		case strings.HasSuffix(m.Site, "DynWrapper).apply"):
			bin["dyn"] = string(m.Bin)
		case strings.HasSuffix(m.Site, "EscapeWrapper).apply"):
			bin["escape"] = string(m.Bin)
		case strings.HasSuffix(m.Site, "SibWrapper).apply"):
			bin["sib"] = string(m.Bin)
		}
	}
	if bin["empty"] != string(frontier.BinB) {
		t.Errorf("the provably-empty middleware loop should predict B (reclaimable), got %q", bin["empty"])
	}
	for _, k := range []string{"dyn", "escape", "sib"} {
		if bin[k] != string(frontier.BinA) {
			t.Errorf("the %s middleware loop is not provably reclaimable; want A, got %q", k, bin[k])
		}
	}
}
