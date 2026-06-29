package reclaim_test

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/reclaim"
)

// edgeSet indexes recovered edges as From->To for assertions.
func mwEdges(t *testing.T, fixture string) (map[string]map[string]bool, []reclaim.MiddlewareSeam) {
	t.Helper()
	res := analyzeFixture(t, fixture)
	mw := reclaim.MiddlewareChain(res)
	got := map[string]map[string]bool{}
	for _, e := range mw.Edges {
		if e.Via != reclaim.ViaMiddlewareChain {
			t.Errorf("edge %v not attributed to the middleware reclaimer (via=%q)", e, e.Via)
		}
		if got[e.From] == nil {
			got[e.From] = map[string]bool{}
		}
		got[e.From][e.To] = true
	}
	return got, mw.ResolvedEmpty
}

func hasEdgeSuffix(edges map[string]map[string]bool, fromSuffix, toSuffix string) bool {
	for from, tos := range edges {
		if !strings.HasSuffix(from, fromSuffix) {
			continue
		}
		for to := range tos {
			if strings.HasSuffix(to, toSuffix) {
				return true
			}
		}
	}
	return false
}

func hasSeam(seams []reclaim.MiddlewareSeam, siteSuffix string) bool {
	for _, s := range seams {
		if strings.HasSuffix(s.Site, siteSuffix) {
			return true
		}
	}
	return false
}

// On the EMPTY-middleware wrapper (the issue's reproducer / the nil-in-prod oapi-codegen
// shape, factored into an `apply` method), the reclaimer recovers the FACTORED terminal:
// the route method dispatches `apply(handler).ServeHTTP(...)`, and with the loop dead the
// business handler runs directly — so caller→business is recovered (GetItems→businessGetItems,
// PostItems→businessPostItems). Because the set is provably empty AND every terminal was
// recovered, the loop's UnresolvedCall seam is fully resolved (ResolvedEmpty names apply).
func TestMiddlewareChainEmptyRecoversAndClears(t *testing.T) {
	edges, seams := mwEdges(t, "mwchainsvc")

	if !hasEdgeSuffix(edges, "EmptyWrapper).GetItems", "mwchainsvc.businessGetItems") {
		t.Errorf("empty wrapper: want recovered GetItems→businessGetItems; got %v", edges)
	}
	if !hasEdgeSuffix(edges, "EmptyWrapper).PostItems", "mwchainsvc.businessPostItems") {
		t.Errorf("empty wrapper: want recovered PostItems→businessPostItems; got %v", edges)
	}
	if !hasSeam(seams, "EmptyWrapper).apply") {
		t.Errorf("empty wrapper: the empty middleware loop should be a resolved seam; got %v", seams)
	}
}

// On the KNOWN non-empty wrapper (HandlerMiddlewares wired from a const slice literal
// {auth, logging}), the reclaimer resolves `mw(h)` to the concrete middleware funcs
// (apply→auth, apply→logging) AND recovers the business terminal (Route→businessGetItems) —
// but it does NOT clear the seam: a non-empty middleware body re-dispatches through its own
// next.ServeHTTP, a residual hop this pass leaves disclosed (reconnect-but-disclose).
func TestMiddlewareChainKnownResolvesButDiscloses(t *testing.T) {
	edges, seams := mwEdges(t, "mwchainsvc")

	if !hasEdgeSuffix(edges, "KnownWrapper).apply", "mwchainsvc.auth") {
		t.Errorf("known wrapper: want apply→auth (resolved middleware); got %v", edges)
	}
	if !hasEdgeSuffix(edges, "KnownWrapper).apply", "mwchainsvc.logging") {
		t.Errorf("known wrapper: want apply→logging (resolved middleware); got %v", edges)
	}
	if !hasEdgeSuffix(edges, "KnownWrapper).Route", "mwchainsvc.businessGetItems") {
		t.Errorf("known wrapper: want Route→businessGetItems (terminal); got %v", edges)
	}
	if hasSeam(seams, "KnownWrapper).apply") {
		t.Errorf("known wrapper: a non-empty middleware loop must stay disclosed, not be cleared; got %v", seams)
	}
}

// The APPEND-chain shape (hand-written stacks `append` known funcs): the reclaimer traces the
// append chain on the field to its complete element set {authA, logA} and recovers apply→each.
func TestMiddlewareChainAppendChainResolves(t *testing.T) {
	edges, _ := mwEdges(t, "mwchainsvc")

	if !hasEdgeSuffix(edges, "AppendWrapper).apply", "mwchainsvc.authA") {
		t.Errorf("append chain: want apply→authA; got %v", edges)
	}
	if !hasEdgeSuffix(edges, "AppendWrapper).apply", "mwchainsvc.logA") {
		t.Errorf("append chain: want apply→logA; got %v", edges)
	}
}

// Soundness (acceptance criterion 3): on the DYNAMIC wrapper (HandlerMiddlewares populated
// from an unknown parameter), the element set is not provable, so the reclaimer recovers NO
// middleware edge for it and does NOT clear the seam — a false edge would be a false PROVEN.
// Over-recovery must never happen; abstaining is correct.
func TestMiddlewareChainDynamicStaysBlind(t *testing.T) {
	edges, seams := mwEdges(t, "mwchainsvc")

	for from, tos := range edges {
		if strings.Contains(from, "DynWrapper).apply") {
			t.Errorf("dynamic wrapper: the unprovable middleware loop must recover no apply→mw edge; got %s -> %v", from, tos)
		}
	}
	if hasSeam(seams, "DynWrapper).apply") {
		t.Errorf("dynamic wrapper: an unprovable middleware set must stay blind, not be cleared; got %v", seams)
	}
}

// The INLINE oapi-codegen strict-server shape (strictsvc: the loop, the per-handler closure,
// and ServeHTTP all in one wrapper method, HandlerMiddlewares nil in prod): the reclaimer
// recovers the inline terminal wrapper→$1 (the same edge the strict-server reclaimer
// recovers — they agree at the seam) and, the set being empty, resolves the seam for every
// route.
func TestMiddlewareChainInlineStrictServer(t *testing.T) {
	edges, seams := mwEdges(t, "strictsvc")

	for _, op := range []string{"CreateEventTypeTemplate", "SyncEventTypes", "GetHealth"} {
		if !hasEdgeSuffix(edges, "ServerInterfaceWrapper)."+op, "ServerInterfaceWrapper)."+op+"$1") {
			t.Errorf("inline %s: want recovered wrapper→$1 terminal; got %v", op, edges)
		}
		if !hasSeam(seams, "ServerInterfaceWrapper)."+op) {
			t.Errorf("inline %s: empty middleware loop should be a resolved seam; got %v", op, seams)
		}
	}
}

// On the NON-empty inline loop (reclaimsvc.wrapper.Admin wraps the served closure through
// authMW), the reclaimer resolves the middleware (Admin→authMW) and recovers the served
// terminal (Admin→Admin$1) — but NEVER the sibling closure Admin$2 the method only passes to
// runLater, and never clears the seam (the set is non-empty). This is the R2 boundary: only
// edges real execution can take.
func TestMiddlewareChainNonEmptyInlineNoFalsePositive(t *testing.T) {
	edges, seams := mwEdges(t, "reclaimsvc")

	if !hasEdgeSuffix(edges, "wrapper).Admin", "reclaimsvc.authMW") {
		t.Errorf("reclaimsvc: want Admin→authMW (resolved middleware); got %v", edges)
	}
	if !hasEdgeSuffix(edges, "wrapper).Admin", "wrapper).Admin$1") {
		t.Errorf("reclaimsvc: want Admin→Admin$1 (served terminal); got %v", edges)
	}
	for from, tos := range edges {
		for to := range tos {
			if strings.Contains(to, "Admin$2") {
				t.Errorf("R2 violation: connected the unserved sibling closure %s -> %s", from, to)
			}
		}
	}
	if hasSeam(seams, "wrapper).Admin") {
		t.Errorf("reclaimsvc: a non-empty middleware loop must stay disclosed; got %v", seams)
	}
}

// Soundness (the completeness guard): when the middleware field's slice ESCAPES into a
// helper that could mutate its backing array past the store walk, the set is not provable —
// even though no element is stored. The reclaimer must abstain (no edge, no clear) rather
// than assume empty; assuming empty here would be a false PROVEN if the helper writes a
// middleware in.
func TestMiddlewareChainEscapingFieldAbstains(t *testing.T) {
	edges, seams := mwEdges(t, "mwchainsvc")

	for from, tos := range edges {
		if strings.Contains(from, "EscapeWrapper") {
			t.Errorf("escaping field: must recover no edge; got %s -> %v", from, tos)
		}
	}
	if hasSeam(seams, "EscapeWrapper).apply") {
		t.Errorf("escaping field: an un-enumerable field must stay blind, not be cleared; got %v", seams)
	}
}

// Soundness / no false positives: services with no middleware-application loop yield nothing.
// A reclaimer that fired on any range-over-funcs loop, or any ServeHTTP dispatch, would emit
// spurious edges here.
func TestMiddlewareChainNoFalsePositives(t *testing.T) {
	for _, name := range []string{"loansvc", "oapisvc", "txrunnersvc"} {
		res := analyzeFixture(t, name)
		mw := reclaim.MiddlewareChain(res)
		if len(mw.Edges) != 0 || len(mw.ResolvedEmpty) != 0 {
			t.Errorf("%s has no middleware-application loop; want nothing recovered, got %d edges / %d seams",
				name, len(mw.Edges), len(mw.ResolvedEmpty))
		}
	}
}

// Determinism (the prime directive): the recovered edge and resolved-seam sequences are
// byte-identical across repeated runs, even though the field-element walk ranges
// ssautil.AllFunctions (a map). The set is sorted on intrinsic FQNs, so order does not vary.
func TestMiddlewareChainDeterministic(t *testing.T) {
	res := analyzeFixture(t, "mwchainsvc")
	first := reclaim.MiddlewareChain(res)
	for i := 0; i < 5; i++ {
		got := reclaim.MiddlewareChain(res)
		if len(got.Edges) != len(first.Edges) {
			t.Fatalf("run %d: edge count changed %d != %d", i, len(got.Edges), len(first.Edges))
		}
		for j := range got.Edges {
			if got.Edges[j] != first.Edges[j] {
				t.Fatalf("run %d: edge order/content changed at %d: %v != %v", i, j, got.Edges[j], first.Edges[j])
			}
		}
		if len(got.ResolvedEmpty) != len(first.ResolvedEmpty) {
			t.Fatalf("run %d: resolved-seam count changed", i)
		}
		for j := range got.ResolvedEmpty {
			if got.ResolvedEmpty[j] != first.ResolvedEmpty[j] {
				t.Fatalf("run %d: resolved-seam order/content changed at %d", i, j)
			}
		}
	}
}
