package graphio_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
	"github.com/jyang234/golang-code-graph/internal/static/statictest"
)

func analyzeFixture(t *testing.T) *analyze.Result {
	t.Helper()
	res, err := statictest.Analyze()
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	return res
}

// TestGraphIncludesDBEdges is the complement of the boundary contract's DB
// exclusion: the non-gated graph DOES show DB operations.
func TestGraphIncludesDBEdges(t *testing.T) {
	g, err := graphio.Build(analyzeFixture(t), "")
	if err != nil {
		t.Fatal(err)
	}
	var dbEdges []string
	for _, e := range g.Edges {
		if strings.HasPrefix(e.To, "boundary:db ") {
			dbEdges = append(dbEdges, e.To)
		}
	}
	if len(dbEdges) == 0 {
		t.Fatal("graph view should include DB boundary edges")
	}
	// The SQL op and table should be extracted, e.g. "boundary:db SELECT applicants".
	var sawTable bool
	for _, e := range dbEdges {
		if strings.Contains(e, "applicants") || strings.Contains(e, "ledger") {
			sawTable = true
		}
	}
	if !sawTable {
		t.Errorf("DB edges did not resolve a table: %v", dbEdges)
	}
}

func TestGraphHasFirstPartyNodesWithSignatures(t *testing.T) {
	g, err := graphio.Build(analyzeFixture(t), "")
	if err != nil {
		t.Fatal(err)
	}
	byFQN := map[string]graphio.Node{}
	for _, n := range g.Nodes {
		byFQN[n.FQN] = n
	}
	create, ok := byFQN["(*example.com/loansvc/internal/handler.App).Create"]
	if !ok {
		t.Fatal("handler.App.Create node missing")
	}
	if !strings.Contains(create.Sig, "ResponseWriter") {
		t.Errorf("Create node signature looks wrong: %q", create.Sig)
	}
	if create.Tier != 1 {
		t.Errorf("Create (an entry handler) tier = %d, want 1", create.Tier)
	}
}

// TestNodeTierFromOutgoingEdges proves a non-root function is tiered by what it
// does, not by what it is: a function that publishes surfaces as tier 1 and a
// pure-compute constructor stays tier 3. Before node-tier-from-edges, every
// non-root node was stuck at the compute tier because it was classified by a
// self-edge ("is this function itself a publish?") rather than by the boundaries
// it reaches.
func TestNodeTierFromOutgoingEdges(t *testing.T) {
	g, err := graphio.Build(analyzeFixture(t), "")
	if err != nil {
		t.Fatal(err)
	}
	byFQN := map[string]graphio.Node{}
	for _, n := range g.Nodes {
		byFQN[n.FQN] = n
	}
	// Evaluate is a non-root function that publishes loan.approved/declined; its
	// most consequential outgoing edge is the tier-1 publish.
	if pub := byFQN["(*example.com/loansvc/internal/origination.Evaluator).Evaluate"]; pub.Tier != 1 {
		t.Errorf("publisher node Evaluate tier = %d, want 1 (derived from its publish edges)", pub.Tier)
	}
	// A pure constructor reaches no boundary → it falls back to the compute tier.
	if ctor := byFQN["example.com/loansvc/internal/store.New"]; ctor.Tier != 3 {
		t.Errorf("pure-compute constructor store.New tier = %d, want 3", ctor.Tier)
	}
}

// TestDBReaderTieredByQueryNotScan proves a DB read is tier 2 (ext-read), not
// inflated to tier 1 by the result-cursor Scan call, and that Scan does not leak
// as a DB boundary edge. This is the read-vs-write distinction: a SELECT is
// tier 2, a mutation tier 1.
func TestDBReaderTieredByQueryNotScan(t *testing.T) {
	g, err := graphio.Build(analyzeFixture(t), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range g.Edges {
		if strings.Contains(e.To, "boundary:db Scan") {
			t.Errorf("result-cursor Scan leaked as a DB boundary edge: %q", e.To)
		}
	}
	byFQN := map[string]graphio.Node{}
	for _, n := range g.Nodes {
		byFQN[n.FQN] = n
	}
	if rd := byFQN["(*example.com/loansvc/internal/store.Loans).SelectApplicant"]; rd.Tier != 2 {
		t.Errorf("DB reader SelectApplicant tier = %d, want 2 (a read, not inflated by Scan)", rd.Tier)
	}
	if wr := byFQN["(*example.com/loansvc/internal/store.Loans).InsertLedger"]; wr.Tier != 1 {
		t.Errorf("DB writer InsertLedger tier = %d, want 1 (mutate)", wr.Tier)
	}
}

// TestGraphShowsConsumeSeam proves the bus consume registration is a visible,
// tier-1 boundary edge (symmetric to the publish seam), not invisible compute.
func TestGraphShowsConsumeSeam(t *testing.T) {
	g, err := graphio.Build(analyzeFixture(t), "")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range g.Edges {
		if e.To == "boundary:bus CONSUME payment.settled" {
			found = true
			if e.Tier != 1 {
				t.Errorf("consume edge tier = %d, want 1", e.Tier)
			}
		}
	}
	if !found {
		t.Error("consume seam (boundary:bus CONSUME payment.settled) is missing from the graph view")
	}
}

// TestEntryScoping checks --entry narrows the graph: the POST flow must exclude a
// function reachable only from the consumer.
func TestEntryScoping(t *testing.T) {
	res := analyzeFixture(t)
	full, err := graphio.Build(res, "")
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := graphio.Build(res, "POST /loan-application")
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Nodes) >= len(full.Nodes) {
		t.Errorf("scoped graph (%d nodes) should be smaller than full (%d)", len(scoped.Nodes), len(full.Nodes))
	}
	if scoped.Entrypoint != "POST /loan-application" {
		t.Errorf("entrypoint = %q", scoped.Entrypoint)
	}
	for _, n := range scoped.Nodes {
		if strings.Contains(n.FQN, "MarkPaid") {
			t.Error("MarkPaid (reached only via the consumer) leaked into the POST scope")
		}
	}
}

func TestEntryNotFound(t *testing.T) {
	_, err := graphio.Build(analyzeFixture(t), "DELETE /nonexistent")
	if err == nil {
		t.Fatal("expected an error for an unknown entry point")
	}
}

func TestGraphDeterministic(t *testing.T) {
	res := analyzeFixture(t)
	g1, err := graphio.Build(res, "")
	if err != nil {
		t.Fatal(err)
	}
	g2, err := graphio.Build(res, "")
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := g1.Marshal()
	b2, _ := g2.Marshal()
	if !bytes.Equal(b1, b2) {
		t.Error("graph view is not deterministic across builds")
	}
}
