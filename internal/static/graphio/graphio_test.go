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

// TestPkgInitCallsAreNotBoundaryEffects pins the isPkgInit guard: a package's
// synthesized init calls the init of every package it imports (Go's init-ordering
// plumbing). Because the loansvc store imports database/sql — a db-classified
// package — store.init calls database/sql.init; without the guard that call is
// mis-rendered as a spurious "boundary:db init" effect (op "init"), a false write
// in the canonical IR. Now that init() is a call-graph root the plumbing edge is
// live, so this regression is reachable: assert no boundary effect carries the op
// "init" (no real db/bus/http API is named init), i.e. no init-ordering call was
// classified as a boundary operation.
func TestPkgInitCallsAreNotBoundaryEffects(t *testing.T) {
	g, err := graphio.Build(analyzeFixture(t), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range g.Edges {
		if e.Boundary == "" {
			continue
		}
		// Boundary labels are "boundary:<kind> <op> …"; the op is the second field.
		fields := strings.Fields(strings.TrimPrefix(e.To, "boundary:"))
		if len(fields) >= 2 && fields[1] == "init" {
			t.Errorf("init-ordering call mis-classified as a boundary effect: from %s -> %q", e.From, e.To)
		}
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

// TestNodePackageIsTypedImportPath proves every first-party node carries its
// defining import path as a typed field, so a consumer never has to recover the
// package by string-splitting the display FQN. A paren-wrapped receiver-method
// node and a plain package-function node both resolve to the import path of their
// defining package — the value the FQN-parse heuristic cannot get right across
// receivers, closures, and generics.
func TestNodePackageIsTypedImportPath(t *testing.T) {
	g, err := graphio.Build(analyzeFixture(t), "")
	if err != nil {
		t.Fatal(err)
	}
	byFQN := map[string]graphio.Node{}
	for _, n := range g.Nodes {
		byFQN[n.FQN] = n
	}
	// A receiver-method node: FQN is paren-wrapped, Package is the bare import path.
	if got := byFQN["(*example.com/loansvc/internal/handler.App).Create"].Package; got != "example.com/loansvc/internal/handler" {
		t.Errorf("receiver-method node Package = %q, want the handler import path", got)
	}
	// A package-level function node.
	if got := byFQN["example.com/loansvc/internal/store.New"].Package; got != "example.com/loansvc/internal/store" {
		t.Errorf("package-function node Package = %q, want the store import path", got)
	}
	// Every first-party node carries a non-empty package — the property a parse
	// cannot guarantee. (All graph nodes are first-party functions with a defining
	// package; a synthetic wrapper with nil fn.Pkg is not in the graph.)
	for _, n := range g.Nodes {
		if n.Package == "" {
			t.Errorf("node %q carries no Package", n.FQN)
		}
	}
}

// TestNodePositionLocatesDeclaration proves the disclosure-only File/Line/EndLine
// fields locate each node at its `func` declaration: a receiver-method node resolves
// to its defining file (RELATIVE to the service dir, so the golden is byte-identical
// across checkouts) and the span runs from the keyword line through the closing brace.
// This is the signal a caller intersects against a git diff to recover the
// author-edited FQN set `review-triage --scope-fqns` consumes.
func TestNodePositionLocatesDeclaration(t *testing.T) {
	g, err := graphio.Build(analyzeFixture(t), "")
	if err != nil {
		t.Fatal(err)
	}
	byFQN := map[string]graphio.Node{}
	for _, n := range g.Nodes {
		byFQN[n.FQN] = n
	}
	// handler.App.Create is declared at internal/handler/application.go:30 and its
	// body closes at line 43 (the func keyword through the closing brace).
	create := byFQN["(*example.com/loansvc/internal/handler.App).Create"]
	if create.File != "internal/handler/application.go" {
		t.Errorf("Create File = %q, want the relative-to-service-dir path", create.File)
	}
	if create.Line != 30 || create.EndLine != 43 {
		t.Errorf("Create span = %d..%d, want 30..43", create.Line, create.EndLine)
	}
	// Every source-backed node carries a usable span: a relative (never absolute,
	// never "..") file, a positive start line, and an end at or past the start. (All
	// loansvc graph nodes are syntax-backed; a synthetic wrapper with no AST omits all
	// three together, which omitempty handles — there are none in this fixture.)
	for _, n := range g.Nodes {
		if n.File == "" || n.Line <= 0 || n.EndLine < n.Line {
			t.Errorf("node %q has an unusable span: file=%q line=%d end=%d", n.FQN, n.File, n.Line, n.EndLine)
		}
		if strings.HasPrefix(n.File, "..") || strings.HasPrefix(n.File, "/") {
			t.Errorf("node %q File = %q is not relative to the service dir", n.FQN, n.File)
		}
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

// TestEffectOrderKeepsCarrierFaultSites locks the loansvc partial-effect
// facts against the regen-laundering failure mode (code-review finding, CX-3
// regression): when a call becomes a DERIVED effect site (the callee
// ALWAYS-publishes), it must remain a fault site for the OTHER effects above
// it — the direct publishes at evaluate.go:89/92 certainly-precede the
// fallible notify call, and those rows must never silently vanish from the
// emitted graph, whatever the goldens say.
func TestEffectOrderKeepsCarrierFaultSites(t *testing.T) {
	res := analyzeFixture(t)
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	const evaluate = "(*example.com/loansvc/internal/origination.Evaluator).Evaluate"
	const notify = "(*example.com/loansvc/internal/origination.Evaluator).notify"
	want := map[string]bool{
		"boundary:bus PUBLISH loan.approved":          false,
		"boundary:bus PUBLISH disbursement.initiated": false,
	}
	for _, f := range g.EffectOrder {
		if f.Fn == evaluate && f.Callee == notify && f.Always {
			if _, ok := want[f.Effect]; ok {
				want[f.Effect] = true
			}
		}
	}
	for effect, found := range want {
		if !found {
			t.Errorf("loansvc lost the fact %q certainly-precedes the fallible %s call", effect, notify)
		}
	}
	for _, f := range g.EffectOrder {
		if f.EffectSite == f.CalleeSite && f.Via != "" {
			t.Errorf("a derived effect paired with its own carrier call: %+v", f)
		}
	}
}
