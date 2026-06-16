package graph

import (
	"path/filepath"
	"reflect"
	"testing"
)

// goldensDir is the committed groundwork graph goldens, relative to this package.
const goldensDir = "../../../testdata/groundwork/goldens"

func loadGolden(t *testing.T, name string) *Index {
	t.Helper()
	g, err := LoadFile(filepath.Join(goldensDir, name))
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return NewIndex(g)
}

// FQNs in the layeredsvc golden, named once.
const (
	hGetUser    = "(*example.com/layeredsvc/internal/handler.Server).GetUser"
	hUpdateUser = "(*example.com/layeredsvc/internal/handler.Server).UpdateUser"
	aGetProfile = "(*example.com/layeredsvc/internal/app.Service).GetProfile"
	aUpdateProf = "(*example.com/layeredsvc/internal/app.Service).UpdateProfile"
	sSelectUser = "(*example.com/layeredsvc/internal/store.Store).SelectUser"
	sUpdateUser = "(*example.com/layeredsvc/internal/store.Store).UpdateUser"
	sInsertAud  = "(*example.com/layeredsvc/internal/store.Store).InsertAudit"
	svcMain     = "example.com/layeredsvc.main"
)

func TestReachableForward(t *testing.T) {
	ix := loadGolden(t, "layeredsvc.graph.json")
	got := ix.Reachable(hGetUser)
	want := []string{
		aGetProfile,
		sSelectUser,
		"example.com/layeredsvc/internal/handler.writeJSON",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Reachable(GetUser) = %v, want %v", got, want)
	}
	// The read path must NOT reach the write store methods — the property a
	// must_not_reach rule will later assert.
	for _, fqn := range got {
		if fqn == sUpdateUser || fqn == sInsertAud {
			t.Errorf("Reachable(GetUser) unexpectedly includes write method %s", fqn)
		}
	}
}

func TestReachingReverse(t *testing.T) {
	ix := loadGolden(t, "layeredsvc.graph.json")
	got := ix.Reaching(sSelectUser)
	want := []string{aGetProfile, hGetUser}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Reaching(SelectUser) = %v, want %v", got, want)
	}
}

func TestSourcesAreEntrypoints(t *testing.T) {
	ix := loadGolden(t, "layeredsvc.graph.json")
	got := ix.Sources()
	want := []string{hGetUser, hUpdateUser, svcMain}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Sources() = %v, want %v", got, want)
	}
}

func TestEntrypointCover(t *testing.T) {
	ix := loadGolden(t, "layeredsvc.graph.json")
	// store.SelectUser is reachable only from the GetUser route.
	if got := ix.EntrypointCover(sSelectUser); !reflect.DeepEqual(got, []string{hGetUser}) {
		t.Errorf("EntrypointCover(SelectUser) = %v, want [GetUser]", got)
	}
	// store.UpdateUser is reachable only from the UpdateUser route.
	if got := ix.EntrypointCover(sUpdateUser); !reflect.DeepEqual(got, []string{hUpdateUser}) {
		t.Errorf("EntrypointCover(UpdateUser) = %v, want [UpdateUser]", got)
	}
	// A source covers itself.
	if got := ix.EntrypointCover(hGetUser); !reflect.DeepEqual(got, []string{hGetUser}) {
		t.Errorf("EntrypointCover(GetUser) = %v, want [GetUser]", got)
	}
}

// EntrypointCoverFrom must produce exactly what EntrypointCover does for every
// node — ground.For relies on the equivalence to feed a single reverse-BFS into
// the cover instead of running the BFS twice per card.
func TestEntrypointCoverFromParity(t *testing.T) {
	ix := loadGolden(t, "loansvc.graph.json")
	for _, fqn := range ix.Nodes() {
		reaching := map[string]bool{}
		for _, r := range ix.Reaching(fqn) {
			reaching[r] = true
		}
		want := ix.EntrypointCover(fqn)
		if got := ix.EntrypointCoverFrom(fqn, reaching); !reflect.DeepEqual(got, want) {
			t.Errorf("EntrypointCoverFrom(%s) = %v, want %v", fqn, got, want)
		}
	}
}

func TestEffects(t *testing.T) {
	ix := loadGolden(t, "layeredsvc.graph.json")
	// The full downstream effect surface of the UpdateUser route is its two writes.
	fqns := append([]string{hUpdateUser}, ix.Reachable(hUpdateUser)...)
	effects := ix.Effects(fqns...)
	if len(effects) != 2 {
		t.Fatalf("want 2 effects on the UpdateUser route, got %d: %v", len(effects), effects)
	}
	for _, e := range effects {
		if !e.IsBoundary() {
			t.Errorf("effect %q is not a boundary edge", e.To)
		}
	}
}

func TestDynamicFrontierAndBlindSpots(t *testing.T) {
	ix := loadGolden(t, "blindsvc.graph.json")

	// The Publish route reaches the dynamic publish — an unsound frontier.
	const publish = "(*example.com/blindsvc/internal/handler.Server).Publish"
	effects := ix.Effects(append([]string{publish}, ix.Reachable(publish)...)...)
	var sawDynamic bool
	for _, e := range effects {
		if e.IsDynamic() {
			sawDynamic = true
		}
	}
	if !sawDynamic {
		t.Errorf("Publish route effects %v missing the <dynamic> frontier", effects)
	}

	// encode.Marshal carries a reflect blind spot.
	const marshal = "example.com/blindsvc/internal/encode.Marshal"
	bs := ix.BlindSpotsAt(marshal)
	if len(bs) != 1 || bs[0].Kind != "reflect" {
		t.Errorf("BlindSpotsAt(Marshal) = %v, want one reflect blind spot", bs)
	}

	// The whole manifest carries the unsafe package disclosure too.
	var kinds []string
	for _, b := range ix.BlindSpots() {
		kinds = append(kinds, b.Kind)
	}
	if !contains(kinds, "reflect") || !contains(kinds, "unsafe") {
		t.Errorf("blind-spot manifest kinds = %v, want both reflect and unsafe", kinds)
	}
}

func TestUnknownFunctionHasNoReach(t *testing.T) {
	ix := loadGolden(t, "layeredsvc.graph.json")
	if got := ix.Reachable("does.not.Exist"); len(got) != 0 {
		t.Errorf("Reachable(unknown) = %v, want empty", got)
	}
	if ix.Has("does.not.Exist") {
		t.Error("Has(unknown) = true, want false")
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// BusEffects must decode the whole statically-named bus surface and count —
// never name — the dynamic effects, against the loansvc golden which has
// publishes, a consume, and one dynamically-named publish.
func TestBusEffects(t *testing.T) {
	ix := loadGolden(t, "loansvc.graph.json")
	effects, dynamic := ix.BusEffects()
	if dynamic != 1 {
		t.Errorf("dynamic = %d, want 1 (the PUBLISH <dynamic> edge)", dynamic)
	}
	byOpEvent := map[string]bool{}
	for _, be := range effects {
		if be.From == "" {
			t.Errorf("effect %v missing From", be)
		}
		byOpEvent[be.Op+" "+be.Event] = true
	}
	for _, want := range []string{
		BusPublish + " loan.approved",
		BusPublish + " loan.declined",
		BusPublish + " disbursement.initiated",
		BusConsume + " payment.settled",
	} {
		if !byOpEvent[want] {
			t.Errorf("BusEffects missing %q (got %v)", want, byOpEvent)
		}
	}
	if byOpEvent[BusPublish+" <dynamic>"] {
		t.Error("a dynamic effect must be counted, never named as an event")
	}
}
