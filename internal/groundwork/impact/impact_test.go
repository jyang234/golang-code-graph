package impact

import (
	"path/filepath"
	"reflect"
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
