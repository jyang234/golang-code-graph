package impact

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
)

const goldensDir = "../../../testdata/groundwork/goldens"

// TestTriageAnnotationNotDuplicated pins the dedup fix on the triage/reach card:
// a seam with two same-kind blind spots echoes its shared annotation once.
func TestTriageAnnotationNotDuplicated(t *testing.T) {
	g := &graph.Graph{
		Algo:  "rta",
		Nodes: []graph.Node{{FQN: "svc.Send"}},
		BlindSpots: []graph.BlindSpot{
			{Kind: "ExternalBoundaryCall", Site: "svc.Send", Detail: "hands off to acme"},
			{Kind: "ExternalBoundaryCall", Site: "svc.Send", Detail: "hands off to stripe"},
		},
		Annotations: []graph.Annotation{
			{Site: "svc.Send", Kind: "ExternalBoundaryCall", Note: "POSTs out to a vendor", By: "dev"},
		},
	}
	card := ForNodes(graph.NewIndex(g), []string{"svc.Send"})
	if len(card.Annotations) != 1 {
		t.Errorf("annotation collected %d times, want 1: %+v", len(card.Annotations), card.Annotations)
	}
	if n := strings.Count(card.Render(), "POSTs out to a vendor"); n != 1 {
		t.Errorf("annotation rendered %d times, want 1:\n%s", n, card.Render())
	}
}

// TestAnnotationEchoedOnTriageCard pins Phase 2 for the triage/reach surface: a
// blind spot's human/AI context rides the card a responder reads, beneath the spot
// it explains.
func TestAnnotationEchoedOnTriageCard(t *testing.T) {
	g := &graph.Graph{
		Algo:  "rta",
		Nodes: []graph.Node{{FQN: "svc.Send"}},
		BlindSpots: []graph.BlindSpot{
			{Kind: "ExternalBoundaryCall", Site: "svc.Send", Detail: "hands off to acme"},
		},
		Annotations: []graph.Annotation{
			{Site: "svc.Send", Kind: "ExternalBoundaryCall", Note: "POSTs to acme.example.com", By: "agent:claude"},
		},
	}
	card := ForNodes(graph.NewIndex(g), []string{"svc.Send"})
	if len(card.Annotations) != 1 {
		t.Fatalf("annotation not collected onto triage card: %+v", card.Annotations)
	}
	out := card.Render()
	for _, want := range []string{"POSTs to acme.example.com", "agent:claude"} {
		if !strings.Contains(out, want) {
			t.Errorf("triage card missing %q:\n%s", want, out)
		}
	}
}

const (
	hGetUser    = "(*example.com/layeredsvc/internal/handler.Server).GetUser"
	sSelectUser = "(*example.com/layeredsvc/internal/store.Store).SelectUser"
)

func index(t *testing.T, name string) *graph.Index {
	t.Helper()
	g, err := graph.LoadFile(filepath.Join(goldensDir, name))
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return graph.NewIndex(g)
}

// O3: blast-radius exactness against a hand-derived expected set, not the
// implementation's own output.
func TestCardForStoreFunction(t *testing.T) {
	ix := index(t, "layeredsvc.graph.json")
	c := ForNodes(ix, []string{sSelectUser})

	if !reflect.DeepEqual(c.Entrypoints, []string{hGetUser}) {
		t.Errorf("entrypoints = %v, want exactly [GetUser]", c.Entrypoints)
	}
	if !reflect.DeepEqual(c.Effects, []string{"boundary:db SELECT users"}) {
		t.Errorf("effects = %v, want the SELECT users effect", c.Effects)
	}
	if len(c.BlindSpots) != 0 {
		t.Errorf("layeredsvc is clean; blind spots = %v", c.BlindSpots)
	}
}

// O4: a card whose traversal touches blind territory always discloses it.
func TestCardDisclosesBlindSpots(t *testing.T) {
	ix := index(t, "blindsvc.graph.json")
	var withDynamic string
	for _, fqn := range ix.Nodes() {
		for _, e := range ix.Effects(fqn) {
			if e.IsDynamic() {
				withDynamic = e.From
			}
		}
	}
	if withDynamic == "" {
		t.Fatal("blindsvc golden has no dynamic effect; fixture drifted")
	}
	c := ForNodes(ix, []string{withDynamic})
	if len(c.BlindSpots) == 0 {
		t.Fatal("card crossed a <dynamic> effect but disclosed no blind spot")
	}
}

// A reverse reach that crosses a HighFanOut dispatch over-approximates the
// implicated-entrypoint set; the card marks the count as an upper bound (F3) and
// discloses the dispatch blind spot it crossed.
func TestCardCoverOverApproxAcrossDispatch(t *testing.T) {
	const ep = "example.com/svc/internal/api.main"
	const dispatch = "(*example.com/svc/internal/api.Wrapper).Dispatch"
	const target = "(*example.com/svc/internal/app.Service).Handle"
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: ep, Sig: "func()", Tier: 1},
			{FQN: dispatch, Sig: "func()", Tier: 1},
			{FQN: target, Sig: "func()", Tier: 1},
		},
		Edges: []graph.Edge{
			{From: ep, To: dispatch, Tier: 2},
			{From: dispatch, To: target, Tier: 2},
		},
		// The dispatch site fanned out to many implementations.
		BlindSpots: []graph.BlindSpot{{Kind: graph.KindHighFanOut, Site: dispatch, Detail: "resolved to 47 callees"}},
	}
	ix := graph.NewIndex(g)
	c := ForNodes(ix, []string{target})

	if !c.CoverOverApprox {
		t.Fatalf("cover crossed a HighFanOut dispatch but CoverOverApprox is false")
	}
	if !strings.Contains(c.Render(), "over-approx via dispatch") {
		t.Fatalf("render must annotate the over-approximated entrypoint cover:\n%s", c.Render())
	}
	var disclosed bool
	for _, b := range c.BlindSpots {
		if b.Kind == graph.KindHighFanOut {
			disclosed = true
		}
	}
	if !disclosed {
		t.Fatalf("HighFanOut on the reverse reach must be disclosed; got %v", c.BlindSpots)
	}
}

// A FORWARD reach that crosses a HighFanOut dispatch over-approximates the reachable-
// effect set (a shared runner fans onto every sibling closure); the card marks it as an
// upper bound and discloses it, in parity with the CLI `reach` lens. The dual of
// TestCardCoverOverApproxAcrossDispatch — here the seam is on the way OUT, not in.
func TestCardEffectsOverApproxAcrossDispatch(t *testing.T) {
	const src = "example.com/svc/internal/app.Command"
	const runner = "(*example.com/svc/internal/tx.UnitOfWork).RunInTx"
	const sibling = "(*example.com/svc/internal/app.Command).Handle$1"
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: src, Sig: "func()", Tier: 1},
			{FQN: runner, Sig: "func()", Tier: 1},
			{FQN: sibling, Sig: "func()", Tier: 1},
		},
		Edges: []graph.Edge{
			{From: src, To: runner, Tier: 2},
			{From: runner, To: sibling, Tier: 2},
			{From: sibling, To: "boundary:db INSERT t", Tier: 2, Boundary: "outbound-sync"},
		},
		// The shared runner's single invoke site fanned onto many sibling closures.
		BlindSpots: []graph.BlindSpot{{Kind: graph.KindHighFanOut, Site: runner, Detail: "resolved to 12 closures"}},
	}
	ix := graph.NewIndex(g)
	c := ForNodes(ix, []string{src})

	if !c.EffectsOverApprox {
		t.Fatalf("forward cone crossed a HighFanOut dispatch but EffectsOverApprox is false")
	}
	if !strings.Contains(c.Render(), "sibling-closure effects past a HighFanOut seam") {
		t.Fatalf("render must annotate the over-approximated effect set:\n%s", c.Render())
	}
}

// O2: card determinism across runs.
func TestCardDeterministic(t *testing.T) {
	ix := index(t, "layeredsvc.graph.json")
	a, b := ForNodes(ix, []string{sSelectUser}), ForNodes(ix, []string{sSelectUser})
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("non-deterministic card: %+v vs %+v", a, b)
	}
}

// O1: each symptom kind resolves; ambiguity returns candidates, never a guess;
// a <dynamic> near-match is flagged as possible.
func TestResolvers(t *testing.T) {
	lay := index(t, "layeredsvc.graph.json")

	if r := ResolveFrame(lay, "example.com/layeredsvc/internal/handler.(*Server).GetUser"); !reflect.DeepEqual(r.Matches, []string{hGetUser}) {
		t.Errorf("runtime-frame form resolved to %v", r.Matches)
	}
	if r := ResolveFrame(lay, "UpdateUser"); !r.Ambiguous || len(r.Matches) != 2 {
		t.Errorf("suffix UpdateUser should be ambiguous (handler + store), got %v", r.Matches)
	}
	// The suffix contract is token-bounded: "User" must not match GetUser.
	if r := ResolveFrame(lay, "User"); len(r.Matches) != 0 {
		t.Errorf("bare 'User' matched %v; suffixes must start at a token boundary", r.Matches)
	}
	if r := ResolveFrame(lay, "Server).GetUser"); !reflect.DeepEqual(r.Matches, []string{hGetUser}) {
		t.Errorf("paren-bounded suffix resolved to %v", r.Matches)
	}
	if r := ResolveTable(lay, "users"); !r.Ambiguous || len(r.Matches) != 2 {
		t.Errorf("table users = %v, want the two store functions", r.Matches)
	}
	if r := ResolveTable(lay, "no_such_table"); len(r.Matches) != 0 || len(r.Possible) != 0 {
		t.Errorf("unknown table resolved to %v / %v", r.Matches, r.Possible)
	}

	blind := index(t, "blindsvc.graph.json")
	if r := ResolveEvent(blind, "user.created"); len(r.Matches) == 0 {
		t.Errorf("event user.created resolved to nothing")
	}
	// An event the graph cannot name statically: the dynamic publisher is a
	// possible match, flagged, never silently included in Matches.
	if r := ResolveEvent(blind, "user.deleted"); len(r.Matches) != 0 || len(r.Possible) == 0 {
		t.Errorf("unknown event: matches=%v possible=%v, want only possible (the <dynamic> publisher)", r.Matches, r.Possible)
	}
}

// Route resolution is segment-aware and never guesses: param wildcards on
// either side, method-optional roots (stdlib HandleFunc), mount-prefix
// tolerance, ambiguity flagged, and uncovered routers absent → loud no-match.
func TestResolveRoute(t *testing.T) {
	loans := index(t, "loansvc.graph.json")
	const create = "(*example.com/loansvc/internal/handler.App).Create"
	const status = "(*example.com/loansvc/internal/handler.App).Status"

	if r := ResolveRoute(loans, "POST /loan-application"); !reflect.DeepEqual(r.Matches, []string{create}) {
		t.Errorf("exact route resolved to %v", r.Matches)
	}
	// Mount prefix + concrete path-param: the alert's URL, not the registration literal.
	if r := ResolveRoute(loans, "GET /api/v1/loan-application/8842/status"); !reflect.DeepEqual(r.Matches, []string{status}) {
		t.Errorf("prefixed concrete route resolved to %v", r.Matches)
	}
	// Wrong method must not match a method-bearing root.
	if r := ResolveRoute(loans, "DELETE /loan-application"); len(r.Matches) != 0 {
		t.Errorf("DELETE matched %v", r.Matches)
	}
	if r := ResolveRoute(loans, "GET /no/such/route"); len(r.Matches) != 0 {
		t.Errorf("unknown route resolved to %v", r.Matches)
	}

	// A method-less stdlib root matches any queried method.
	oblig := index(t, "obligsvc.graph.json")
	r := ResolveRoute(oblig, "POST /transfer")
	if len(r.Matches) != 1 {
		t.Errorf("method-less root did not match a method-ful query: %v", r.Matches)
	}

	// Two handlers behind one path (different methods), queried without a
	// method: all candidates, flagged — never a guess.
	g, _ := graph.LoadFile(goldensDir + "/loansvc.graph.json")
	g.Entrypoints = append(g.Entrypoints,
		graph.Entrypoint{Kind: "http", Name: "GET /loan-application", Fn: status})
	if r := ResolveRoute(graph.NewIndex(g), "/loan-application"); !r.Ambiguous || len(r.Matches) != 2 {
		t.Errorf("method-less query over two methods should be ambiguous: %v", r.Matches)
	}
}

// The consumer side of an event resolves to the actual handler (the
// entrypoints join), not just the registration site the CONSUME edge points
// from.
func TestResolveEventPrefersHandler(t *testing.T) {
	ix := index(t, "loansvc.graph.json")
	r := ResolveEvent(ix, "payment.settled")
	found := false
	for _, m := range r.Matches {
		if m == "(*example.com/loansvc/internal/consumer.Payments).OnSettled" {
			found = true
		}
	}
	if !found {
		t.Errorf("consumer handler missing from %v", r.Matches)
	}
}

// IT-2 exit criterion: "peer P down" names exactly the entrypoints whose paths
// cross a boundary:P edge — asserted against a hand-derived expected set from
// the loansvc golden.
func TestFaultPeerDown(t *testing.T) {
	ix := index(t, "loansvc.graph.json")
	r := ResolvePeer(ix, "credit-bureau")
	if !reflect.DeepEqual(r.Matches, []string{"(*example.com/loansvc/internal/client.Bureau).Score"}) {
		t.Fatalf("peer credit-bureau resolved to %v", r.Matches)
	}
	c := ForFault(ix, r.Matches)
	if !c.Fault {
		t.Fatal("fault framing not set")
	}
	want := []string{"(*example.com/loansvc/internal/origination.Evaluator).Evaluate$2"}
	if !reflect.DeepEqual(c.Entrypoints, want) {
		t.Errorf("degraded entrypoints = %v, want %v (hand-derived)", c.Entrypoints, want)
	}
	if !reflect.DeepEqual(c.Effects, []string{"boundary:credit-bureau GET /score/{id}"}) {
		t.Errorf("effects = %v", c.Effects)
	}
}

// An event symptom matches whichever side the service has — the publisher of
// an outbound event, the consumer registrar of an inbound one — and the
// <dynamic> publisher is offered as a flagged possible match for the outbound
// case (it might be the event under a runtime-chosen name).
func TestFaultEventMissing(t *testing.T) {
	ix := index(t, "loansvc.graph.json")

	r := ResolveEvent(ix, "loan.approved") // published by this service
	if len(r.Matches) != 1 || len(r.Possible) == 0 {
		t.Fatalf("loan.approved: matches=%v possible=%v, want one publisher plus the <dynamic> publisher flagged", r.Matches, r.Possible)
	}
	if c := ForFault(ix, r.Matches); len(c.Entrypoints) == 0 {
		t.Error("publish-side fault card names no entrypoints")
	}

	r = ResolveEvent(ix, "payment.settled") // consumed by this service
	if len(r.Matches) == 0 {
		t.Fatalf("payment.settled resolved to nothing; the consumer registrar should match")
	}
}

// IT-3 exit criterion: the disburse scenario reproduces. Fail the charge call
// and the card reports loan.approved as committed-before-the-fault — certainly
// where the publish dominates the charge, possibly where it sits on one arm.
func TestFaultPartialEffects(t *testing.T) {
	ix := index(t, "obligsvc.graph.json")
	r := ResolveFrame(ix, "Charge")
	if len(r.Matches) != 1 {
		t.Fatalf("Charge resolved to %v", r.Matches)
	}
	c := ForFault(ix, r.Matches)

	wantIn := func(list []string, sub, which string) {
		t.Helper()
		for _, s := range list {
			if strings.Contains(s, sub) {
				return
			}
		}
		t.Errorf("%s = %v, want an entry containing %q", which, list, sub)
	}
	wantIn(c.CertainlyCommitted, "loan.approved", "CertainlyCommitted")
	wantIn(c.PossiblyCommitted, "loan.approved", "PossiblyCommitted")
	if !strings.Contains(c.Render(), "CERTAINLY committed") {
		t.Error("rendered fault card hides the partial-effect section")
	}

	// Negative: a fault at a function that is no fact's fallible callee
	// commits nothing.
	clean := ForFault(ix, []string{"example.com/obligsvc/internal/audit.Write"})
	if len(clean.PossiblyCommitted)+len(clean.CertainlyCommitted) != 0 {
		t.Errorf("non-fallible-callee fault reported committed effects: %v / %v",
			clean.PossiblyCommitted, clean.CertainlyCommitted)
	}

	// And the plain (non-fault) card never carries the sections.
	plain := ForNodes(ix, r.Matches)
	if len(plain.PossiblyCommitted)+len(plain.CertainlyCommitted) != 0 {
		t.Error("non-fault card carries partial-effect sections")
	}
}

// The fault card states its epistemic scope next to the evidence — including
// (especially) when the partial-effect sections are empty, so their absence
// cannot read as an all-clear. The plain card carries no fault scope block.
func TestFaultCardStatesScope(t *testing.T) {
	ix := index(t, "layeredsvc.graph.json")
	fault := ForFault(ix, []string{sSelectUser}).Render() // no effect_order facts here
	for _, want := range []string{"causes outside the code", "same-function orderings only"} {
		if !strings.Contains(fault, want) {
			t.Errorf("fault card missing scope statement %q", want)
		}
	}
	if plain := ForNodes(ix, []string{sSelectUser}).Render(); strings.Contains(plain, "same-function orderings") {
		t.Error("plain card carries the fault scope block")
	}
}
