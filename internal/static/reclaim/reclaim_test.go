package reclaim_test

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

// Integration: folding the reclaimed edges into the graph closes the strict-server
// STRUCTURAL seam — every route reaches its effects, so the frontier reports zero
// attribution loss and no severed-closure / starved-entrypoint markers. The residual
// middleware-application UnresolvedCall seams that StrictServer leaves (it does not touch the
// `mw(h)` loop — TestStrictServerReconnectedButDisclosed) are disclosed as B, not A: on the
// faithful oapi-codegen shape the middleware set enters through the `HandlerMiddlewares:
// options.Middlewares` param-field copy, which the reclaimer proves empty transitively
// (TestMiddlewareChainOapiBootstrapClears), so the frontier names them reclaimable by
// --reclaim-middleware rather than irreducible.
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
	// The strict-server STRUCTURAL seam is fully closed: no severed-closure / starved-
	// entrypoint marker survives, and every B marker that remains is a middleware UnresolvedCall
	// seam (reclaimable by --reclaim-middleware via the transitive empty-set proof, not irreducible).
	for _, m := range r.Markers {
		if m.Kind == "severed-closure" || m.Kind == "starved-entrypoint" {
			t.Errorf("reclaimed graph must carry no strict-server seam marker, got %+v", m)
		}
		if m.Bin == frontier.BinB && m.Kind != string(blindspots.UnresolvedCall) {
			t.Errorf("after strict-server reclaim the only B markers should be middleware UnresolvedCall seams, got %+v", m)
		}
	}
	// The reclaimed routes now reach their effects, so they are neither severed nor
	// unconfirmed.
	if len(r.UnconfirmedRoutes) != 0 {
		t.Errorf("reclaim must leave no unconfirmed routes, got %v", r.UnconfirmedRoutes)
	}
}

// TestStrictServerReconnectedButDisclosed pins the duality a C3 consumer asks about:
// after --reclaim, the strict-server dispatch→closure edge is RECONNECTED (the frontier
// is closed, TestApplyReclaimersClosesSeam) yet a residual func() blind spot still
// rides the manifest — the per-route MiddlewareFunc application over the (nil-in-prod)
// HandlerMiddlewares slice. That residual is correctly LEFT AS SIGNAL (empty Severity,
// not tiered trivial): a middleware value is read from a field assigned elsewhere and
// CAN bear an effect, so demoting it would over-claim benignity. Its Detail names the
// DEFINED type (api.MiddlewareFunc), so a census can group these framework-dispatch
// seams by type without the producer pre-judging them. The component edge is recovered;
// the disclosure is honest about what the middleware hop still hides.
func TestStrictServerReconnectedButDisclosed(t *testing.T) {
	res := analyzeFixture(t, "strictsvc")
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if added := graphio.ApplyReclaimers(g, res); added != 3 {
		t.Fatalf("want 3 edges folded in, got %d", added)
	}
	var residual int
	for _, b := range g.BlindSpots {
		if b.Kind != blindspots.UnresolvedCall {
			continue
		}
		if !strings.Contains(b.Detail, "MiddlewareFunc") {
			t.Errorf("residual strict-server seam should name the defined MiddlewareFunc type, got %q", b.Detail)
		}
		if b.Severity == blindspots.SeverityTrivial {
			t.Errorf("a middleware-application seam can bear an effect; it must NOT be tiered trivial, got %q", b.Severity)
		}
		residual++
	}
	if residual == 0 {
		t.Fatal("strict-server reclaim should reconnect the closure yet still DISCLOSE the residual middleware seam; found none")
	}
}

// On the tx-runner fixture the TxClosure reclaimer recovers the enclosing-fn→closure
// edges the builder lost at the higher-order tx-runner seam: both the GENERIC wrapper
// (RunInTxResult[T]) and the DIRECT (u.RunInTx(closure)) commands, each From the
// enclosing function To its OWN `$1` closure, attributed via=tx-closure. The R2
// NEGATIVE — a closure handed to a runner that only STORES it (StashCommand→Register)
// — is NOT connected: no execution path invokes it.
func TestTxClosureReclaimsGenericWrapper(t *testing.T) {
	edges := reclaim.TxClosure(analyzeFixture(t, "txrunnersvc"))

	got := map[string]string{} // From -> To
	for _, e := range edges {
		if e.Via != reclaim.ViaTxClosure {
			t.Errorf("edge %v not attributed to the tx-closure reclaimer (via=%q)", e, e.Via)
		}
		// Every recovered edge connects a function to its OWN closure.
		if base := trimClosureSuffix(e.To); base != e.From {
			t.Errorf("edge does not connect a function to its own closure: %s -> %s", e.From, e.To)
		}
		// R2 negative: the stored-but-never-invoked closure must never be a target.
		if strings.Contains(e.From, "StashCommand") || strings.Contains(e.To, "StashCommand") {
			t.Errorf("R2 violation: connected a stored-but-never-invoked closure: %s -> %s", e.From, e.To)
		}
		// Soundness: MaybeCommand dispatches through an interface with a LAZY impl that
		// never invokes the closure, so it must NOT be connected (every impl must invoke).
		if strings.Contains(e.From, "MaybeCommand") || strings.Contains(e.To, "MaybeCommand") {
			t.Errorf("soundness violation: connected a closure whose interface runner has a non-invoking impl: %s -> %s", e.From, e.To)
		}
		got[e.From] = e.To
	}

	// The GENERIC wrapper command (RunInTxResult[T]) — the measured under-reporter — is
	// recovered: its writing closure is reconnected to the command.
	assertReclaimed(t, got, "CreateSubscriptionCommand).Handle")
	// The DIRECT runner command is recovered too (closure invoked by RunInTx directly).
	assertReclaimed(t, got, "DirectCommand).Handle")
	// The INTERFACE tx-runner command (the dominant real shape): the runner call inside the
	// generic wrapper is interface-dispatched, so recovery requires resolving the interface
	// method to its single concrete impl and proving it invokes the parameter.
	assertReclaimed(t, got, "IfaceCommand).Handle")
	// The VALUE-receiver interface impl (event-bus's dominant shape): prog.MethodValue(*T) is a
	// synthesized promotion wrapper that does not itself invoke the parameter, so without
	// unwrapPromotion the "every impl invokes" guard abstains and this write stays orphaned.
	// ValueRunner shares TxRunner with *SQLRunner, so this also pins the CONTAGION repair: a
	// value-receiver impl must NOT poison the pointer-receiver IfaceCommand above. Differential:
	// this assertion (and IfaceCommand's) fail without the unwrap+dedup fix.
	assertReclaimed(t, got, "ValueIfaceCommand).Handle")
}

// Integration: folding TxClosure's edges into the graph makes the orphaned write
// VISIBLE from the command — the false-clean fix. Before reclaim, the forward reach of
// CreateSubscriptionCommand.Handle never crosses its closure, so the INSERT is invisible;
// after --reclaim it is reachable. base goldens are unaffected because Build never folds.
func TestTxClosureMakesOrphanedWriteVisible(t *testing.T) {
	res := analyzeFixture(t, "txrunnersvc")
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	const handle = "(*example.com/txrunnersvc.CreateSubscriptionCommand).Handle"
	if reachesInsert(g, handle) {
		t.Fatalf("precondition: the generic-wrapper write should be orphaned (invisible) before reclaim")
	}

	added := graphio.ApplyReclaimers(g, res)
	if added == 0 {
		t.Fatal("ApplyReclaimers folded no tx-closure edges")
	}
	taggedFound := false
	for _, e := range g.Edges {
		if e.Via == reclaim.ViaTxClosure {
			taggedFound = true
		}
	}
	if !taggedFound {
		t.Error("reclaimed edges must carry their via=tx-closure provenance in the graph")
	}
	if !reachesInsert(g, handle) {
		t.Error("after --reclaim the command must reach its orphaned INSERT (the false-clean fix)")
	}
}

// reachesInsert reports whether the forward reach of fqn in g crosses an
// event_type_subscriptions INSERT boundary effect.
func reachesInsert(g *graphio.Graph, fqn string) bool {
	reachable := map[string]bool{fqn: true}
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
		if strings.Contains(to, "INSERT") && strings.Contains(to, "event_type_subscriptions") {
			return true
		}
	}
	return false
}

func trimClosureSuffix(fqn string) string {
	if i := strings.LastIndex(fqn, "$"); i >= 0 {
		return fqn[:i]
	}
	return fqn
}

func assertReclaimed(t *testing.T, got map[string]string, fromSuffix string) {
	t.Helper()
	for from, to := range got {
		if strings.HasSuffix(from, fromSuffix) {
			if !strings.Contains(to, "$") {
				t.Errorf("reclaimed To for %s is not a closure: %q", fromSuffix, to)
			}
			return
		}
	}
	t.Errorf("missing recovered tx-closure edge for %s; got %v", fromSuffix, got)
}
