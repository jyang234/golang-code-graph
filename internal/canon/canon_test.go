package canon

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/jyang234/golang-code-graph/capture"
	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/ir"
)

// fixtureParams varies the dimensions a real run varies — absolute clock, the
// applicant id baked into URLs/SQL, and how long the credit-bureau leg takes —
// so the determinism self-test can assert the IR is invariant under all three.
type fixtureParams struct {
	baseMS   int    // absolute time shift
	id       string // applicant/loan id (volatile)
	bureauMS int    // credit-bureau leg duration (fast vs slow)
}

func ms(base, v int) time.Time { return time.Unix(0, 0).Add(time.Duration(base+v) * time.Millisecond) }

// buildFixtureFlow constructs the loansvc happy path as a captured flow:
// evaluateApplication runs a concurrent applicant-read ∥ credit-score pair, then
// the sequential charge → publishes → disburse (ledger then a fire-and-forget
// audit). The internal evaluator/scorer/disburse/auditLog wrappers are tier 3 and
// must be contracted away.
func buildFixtureFlow(p fixtureParams) capture.CapturedFlow {
	b := p.baseMS
	bureauEnd := 25
	if p.bureauMS != 0 {
		bureauEnd = p.bureauMS
	}
	scorerEnd := bureauEnd + 1

	spans := []capture.Span{
		{ID: "root", Kind: ir.KindServer, Status: capture.StatusOK, Start: ms(b, 0), End: ms(b, 100),
			Attrs: map[string]string{"http.request.method": "POST", "http.route": "/loan-application", capture.CorrelationKey: "run"}},

		{ID: "eval", ParentID: "root", Kind: ir.KindInternal, Name: "evaluateApplication", Start: ms(b, 1), End: ms(b, 30),
			Attrs: map[string]string{capture.CorrelationKey: "run"}},
		{ID: "select", ParentID: "eval", Kind: ir.KindClient, Start: ms(b, 2), End: ms(b, 10),
			Attrs: map[string]string{"db.system": "postgresql", "db.statement": "SELECT name, income FROM applicants WHERE id = " + sqlLit(p.id), capture.CorrelationKey: "run"}},
		{ID: "scorer", ParentID: "eval", Kind: ir.KindInternal, Name: "scorer.Score", Start: ms(b, 2), End: ms(b, scorerEnd),
			Attrs: map[string]string{capture.CorrelationKey: "run"}},
		{ID: "bureau", ParentID: "scorer", Kind: ir.KindClient, Start: ms(b, 3), End: ms(b, bureauEnd),
			Attrs: map[string]string{"http.method": "GET", "peer.service": "credit-bureau", "http.target": "/score/" + p.id, capture.CorrelationKey: "run"}},

		{ID: "charge", ParentID: "root", Kind: ir.KindClient, Status: capture.StatusOK, Start: ms(b, 31), End: ms(b, 40),
			Attrs: map[string]string{"http.request.method": "POST", "peer.service": "payment-gw", "http.target": "/charge/" + p.id, capture.CorrelationKey: "run"}},
		{ID: "pubApproved", ParentID: "root", Kind: ir.KindProducer, Start: ms(b, 41), End: ms(b, 42),
			Attrs: map[string]string{"messaging.destination.name": "loan.approved", capture.CorrelationKey: "run"}},
		{ID: "pubDisb", ParentID: "root", Kind: ir.KindProducer, Start: ms(b, 43), End: ms(b, 44),
			Attrs: map[string]string{"messaging.destination.name": "disbursement.initiated", capture.CorrelationKey: "run"}},

		{ID: "disburse", ParentID: "root", Kind: ir.KindInternal, Name: "disburse", Start: ms(b, 45), End: ms(b, 70),
			Attrs: map[string]string{capture.CorrelationKey: "run"}},
		{ID: "ledger", ParentID: "disburse", Kind: ir.KindClient, Start: ms(b, 46), End: ms(b, 50),
			Attrs: map[string]string{"db.system": "postgres", "db.statement": "INSERT INTO ledger (loan_id, amount) VALUES (" + sqlLit(p.id) + ", 5000)", capture.CorrelationKey: "run"}},
		{ID: "auditLog", ParentID: "disburse", Kind: ir.KindInternal, Name: "auditLog", Start: ms(b, 52), End: ms(b, 70),
			Attrs: map[string]string{capture.CorrelationKey: "run"}},
		{ID: "audit", ParentID: "auditLog", Kind: ir.KindClient, Start: ms(b, 53), End: ms(b, 69),
			Attrs: map[string]string{"db.system": "postgres", "db.statement": "INSERT INTO audit_log (loan_id) VALUES (" + sqlLit(p.id) + ")", capture.CorrelationKey: "run"}},
	}
	root := &spans[0]
	return capture.CapturedFlow{
		Flow: "POST /loan-application", Service: "loansvc",
		Trigger: capture.TriggerHTTP, Mode: capture.ModeInProcess,
		Spans: spans, Root: root, Complete: true,
	}
}

func sqlLit(id string) string { return "'" + id + "'" }

func marshal(t *testing.T, tr *ir.CanonicalTrace) []byte {
	t.Helper()
	b, err := tr.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestFixtureShape checks the contracted tree against the spec's worked-example
// structure (not byte-pinned): the tier-3 evaluator is gone and the concurrent
// applicant-read ∥ credit-score pair sits directly under the root in
// canonical-key order, followed by the sequential charge → publishes → ledger →
// audit.
func TestFixtureShape(t *testing.T) {
	tr, err := Canonicalize(buildFixtureFlow(fixtureParams{id: "8412"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	root := tr.Root
	if root.Op != "HTTP POST /loan-application" || root.Tier != 1 {
		t.Fatalf("root = {op:%q tier:%d}", root.Op, root.Tier)
	}
	if len(root.Children) != 6 {
		t.Fatalf("root should have 6 child groups, got %d: %s", len(root.Children), marshal(t, tr))
	}

	g0 := root.Children[0]
	if !g0.Concurrent || len(g0.Members) != 2 {
		t.Fatalf("first group should be the promoted concurrent pair, got %+v", g0)
	}
	if g0.Members[0].Op != "DB postgresql SELECT applicants" {
		t.Errorf("concurrent[0] = %q, want the DB read first (canonical-key order)", g0.Members[0].Op)
	}
	if g0.Members[1].Op != "HTTP GET credit-bureau /score/{id}" {
		t.Errorf("concurrent[1] = %q, want the credit-bureau call promoted from the dropped scorer", g0.Members[1].Op)
	}

	wantSeq := []string{
		"HTTP POST payment-gw /charge/{id}",
		"PUBLISH loan.approved",
		"PUBLISH disbursement.initiated",
		"DB postgres INSERT ledger",
		"DB postgres INSERT audit_log",
	}
	for i, want := range wantSeq {
		g := root.Children[i+1]
		if g.Concurrent || len(g.Members) != 1 {
			t.Fatalf("group %d should be a single sequential step, got %+v", i+1, g)
		}
		if g.Members[0].Op != want {
			t.Errorf("sequential group %d = %q, want %q", i+1, g.Members[0].Op, want)
		}
	}
}

// TestParameterization checks volatile ids are templated out of URLs and SQL.
func TestParameterization(t *testing.T) {
	tr, err := Canonicalize(buildFixtureFlow(fixtureParams{id: "8412"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	read := tr.Root.Children[0].Members[0]
	if got := read.Attrs["db.statement"]; got != "SELECT name , income FROM applicants WHERE id = ?" {
		t.Errorf("db.statement = %q, want literal stripped to ?", got)
	}
	bureau := tr.Root.Children[0].Members[1]
	if bureau.Op != "HTTP GET credit-bureau /score/{id}" {
		t.Errorf("bureau op = %q, want /score/{id}", bureau.Op)
	}
}

// TestDeterminism is the self-test: vary the absolute clock, the id baked into
// URLs and SQL, and the credit-bureau leg's duration (and shuffle the input span
// order). Every variant must canonicalize to byte-identical IR.
func TestDeterminism(t *testing.T) {
	variants := []fixtureParams{
		{baseMS: 0, id: "8412", bureauMS: 25},
		{baseMS: 1_000_000, id: "99999", bureauMS: 25},                         // different clock + id
		{baseMS: 0, id: "8412", bureauMS: 90},                                  // slow credit-bureau leg
		{baseMS: 500, id: "3f2a4b1c-1111-2222-3333-444455556666", bureauMS: 5}, // fast leg, UUID id
	}
	want := marshal(t, mustCanon(t, buildFixtureFlow(variants[0])))
	for _, v := range variants[1:] {
		cf := buildFixtureFlow(v)
		shuffleSpans(cf.Spans)
		fixupRoot(&cf)
		got := marshal(t, mustCanon(t, cf))
		if string(got) != string(want) {
			t.Errorf("variant %+v produced a different IR:\n--- want ---\n%s\n--- got ---\n%s", v, want, got)
		}
	}
}

// TestSelfTestRepeatedCanon runs canon twice on the same capture (the cheapest
// self-test) and asserts byte-identical output.
func TestSelfTestRepeatedCanon(t *testing.T) {
	cf := buildFixtureFlow(fixtureParams{id: "8412"})
	a := marshal(t, mustCanon(t, cf))
	b := marshal(t, mustCanon(t, cf))
	if string(a) != string(b) {
		t.Error("canon is not idempotent on identical input")
	}
}

// TestRefusesIncomplete is the hard stop: a truncated capture is never snapshotted.
func TestRefusesIncomplete(t *testing.T) {
	cf := buildFixtureFlow(fixtureParams{id: "8412"})
	cf.Complete = false
	if _, err := Canonicalize(cf, nil); err != ErrIncomplete {
		t.Fatalf("err = %v, want ErrIncomplete", err)
	}
}

// TestLoopCollapse folds N identical sequential subtrees into one representative
// with a 1..* multiplicity, so item count does not perturb the snapshot.
func TestLoopCollapse(t *testing.T) {
	mkItem := func(id string, start int) capture.Span {
		return capture.Span{ID: id, ParentID: "root", Kind: ir.KindClient, Start: ms(0, start), End: ms(0, start+1),
			Attrs: map[string]string{"db.system": "postgres", "db.statement": "INSERT INTO items (id) VALUES (1)"}}
	}
	spans := []capture.Span{
		{ID: "root", Kind: ir.KindServer, Status: capture.StatusOK, Start: ms(0, 0), End: ms(0, 100),
			Attrs: map[string]string{"http.request.method": "POST", "http.route": "/batch"}},
		mkItem("i1", 1), mkItem("i2", 3), mkItem("i3", 5), mkItem("i4", 7),
	}
	tr := mustCanon(t, capture.CapturedFlow{Flow: "batch", Service: "s", Spans: spans, Root: &spans[0], Complete: true})
	if len(tr.Root.Children) != 1 {
		t.Fatalf("four identical inserts should collapse to one group, got %d", len(tr.Root.Children))
	}
	if tr.Root.Children[0].Multiplicity != "1..*" {
		t.Errorf("multiplicity = %q, want 1..*", tr.Root.Children[0].Multiplicity)
	}
	if len(tr.Discards.Loops) == 0 {
		t.Error("collapsed loop op should be recorded in Discards.Loops")
	}
}

// TestLoopCollapseStableUnderCount confirms 4 vs 40 identical items snapshot the
// same.
func TestLoopCollapseStableUnderCount(t *testing.T) {
	build := func(n int) *ir.CanonicalTrace {
		spans := []capture.Span{{ID: "root", Kind: ir.KindServer, Status: capture.StatusOK, Start: ms(0, 0), End: ms(0, 1000),
			Attrs: map[string]string{"http.request.method": "POST", "http.route": "/batch"}}}
		for i := 0; i < n; i++ {
			spans = append(spans, capture.Span{ID: fmt.Sprintf("i%d", i), ParentID: "root", Kind: ir.KindClient,
				Start: ms(0, 1+2*i), End: ms(0, 2+2*i),
				Attrs: map[string]string{"db.system": "postgres", "db.statement": "INSERT INTO items (id) VALUES (1)"}})
		}
		return mustCanon(t, capture.CapturedFlow{Flow: "batch", Service: "s", Spans: spans, Root: &spans[0], Complete: true})
	}
	if string(marshal(t, build(4))) != string(marshal(t, build(40))) {
		t.Error("loop count leaked into the snapshot")
	}
}

// TestRedaction replaces a configured attribute's volatile value with a
// placeholder and records the key in the manifest.
func TestRedaction(t *testing.T) {
	spans := []capture.Span{
		{ID: "root", Kind: ir.KindServer, Status: capture.StatusOK, Start: ms(0, 0), End: ms(0, 10),
			Attrs: map[string]string{"http.request.method": "GET", "http.route": "/x", "request.id": "7f3a"}},
	}
	tr := mustCanonCfg(t, capture.CapturedFlow{Flow: "f", Service: "s", Spans: spans, Root: &spans[0], Complete: true},
		mustConfig(t, "canon:\n  attributeAllowlist: [\"request.id\"]\n  redactKeys: [\"request.id\"]\n"))
	if got := tr.Root.Attrs["request.id"]; got != "<redacted>" {
		t.Errorf("request.id = %q, want <redacted>", got)
	}
	found := false
	for _, k := range tr.Discards.Redactions {
		if k == "request.id" {
			found = true
		}
	}
	if !found {
		t.Errorf("redacted key not recorded in manifest: %v", tr.Discards.Redactions)
	}
}

// TestInternalKindDBSpanTiered is the regression for DB spans opened as
// internal-kind (some ORMs do this): opkey keys them as DB operations, so the
// classifier must tier them as DB (ext-read = 2 / mutate = 1) and retain them,
// not treat them as ordinary internal compute (tier 3) and drop them.
func TestInternalKindDBSpanTiered(t *testing.T) {
	spans := []capture.Span{
		{ID: "root", Kind: ir.KindServer, Status: capture.StatusOK, Start: ms(0, 0), End: ms(0, 100),
			Attrs: map[string]string{"http.request.method": "GET", "http.route": "/loans/{id}"}},
		{ID: "read", ParentID: "root", Kind: ir.KindInternal, Name: "ormSelect", Start: ms(0, 1), End: ms(0, 5),
			Attrs: map[string]string{"db.system": "postgresql", "db.statement": "SELECT id, status FROM loans WHERE id = $1"}},
		{ID: "write", ParentID: "root", Kind: ir.KindInternal, Name: "ormUpdate", Start: ms(0, 6), End: ms(0, 9),
			Attrs: map[string]string{"db.system": "postgresql", "db.statement": "UPDATE loans SET status = 'paid' WHERE id = $1"}},
	}
	tr := mustCanon(t, capture.CapturedFlow{Flow: "GET /loans/{id}", Service: "loansvc", Spans: spans, Root: &spans[0], Complete: true})
	if len(tr.Root.Children) != 2 {
		t.Fatalf("both DB spans should survive salience, got %d groups: %s", len(tr.Root.Children), marshal(t, tr))
	}
	read := tr.Root.Children[0].Members[0]
	if read.Op != "DB postgresql SELECT loans" || read.Tier != 2 {
		t.Errorf("internal-kind SELECT = {op:%q tier:%d}, want DB op at tier 2", read.Op, read.Tier)
	}
	write := tr.Root.Children[1].Members[0]
	if write.Op != "DB postgresql UPDATE loans" || write.Tier != 1 {
		t.Errorf("internal-kind UPDATE = {op:%q tier:%d}, want DB op at tier 1", write.Op, write.Tier)
	}
}

func shuffleSpans(s []capture.Span) {
	r := rand.New(rand.NewSource(42))
	r.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
}

func mustCanon(t *testing.T, cf capture.CapturedFlow) *ir.CanonicalTrace {
	t.Helper()
	tr, err := Canonicalize(cf, nil)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	return tr
}

func mustCanonCfg(t *testing.T, cf capture.CapturedFlow, cfg *config.Config) *ir.CanonicalTrace {
	t.Helper()
	tr, err := Canonicalize(cf, cfg)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	return tr
}

func mustConfig(t *testing.T, y string) *config.Config {
	t.Helper()
	c, err := config.Load([]byte(y))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return c
}

// fixupRoot re-points cf.Root at the parentless span after the span slice has
// been shuffled, so a reordered input still names the same entry.
func fixupRoot(cf *capture.CapturedFlow) {
	for i := range cf.Spans {
		if cf.Spans[i].ID == "root" {
			cf.Root = &cf.Spans[i]
			return
		}
	}
}
