package impact

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
)

const goldensDir = "../../../testdata/groundwork/goldens"

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
