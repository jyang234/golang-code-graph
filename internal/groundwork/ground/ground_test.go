package ground

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

const goldensDir = "../../../testdata/groundwork/goldens"

func index(t *testing.T, name string) *graph.Index {
	t.Helper()
	g, err := graph.LoadFile(filepath.Join(goldensDir, name))
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return graph.NewIndex(g)
}

// TestAnnotationEchoedOnCard pins Phase 2: the human/AI context attached to a
// blind spot reaches the ground card an agent reads before editing — the note and
// its author render beneath the blind spot they explain.
func TestAnnotationEchoedOnCard(t *testing.T) {
	g := &graph.Graph{
		Algo:  "rta",
		Nodes: []graph.Node{{FQN: "svc.Send"}},
		BlindSpots: []graph.BlindSpot{
			{Kind: "ExternalBoundaryCall", Site: "svc.Send", Detail: "hands off to acme"},
		},
		Annotations: []graph.Annotation{
			{Site: "svc.Send", Kind: "ExternalBoundaryCall", Note: "issues an outbound HTTPS POST to acme", By: "dev@x"},
		},
	}
	card, err := For(graph.NewIndex(g), nil, "svc.Send")
	if err != nil {
		t.Fatal(err)
	}
	if len(card.Annotations) != 1 {
		t.Fatalf("annotation not collected onto card: %+v", card.Annotations)
	}
	out := card.Render()
	for _, want := range []string{"ExternalBoundaryCall svc.Send", "issues an outbound HTTPS POST to acme", "dev@x"} {
		if !strings.Contains(out, want) {
			t.Errorf("ground card missing %q:\n%s", want, out)
		}
	}
	// An annotation whose (site, kind) matches no blind spot on the card is not
	// echoed — context only ever appears beneath the spot it explains.
	g.Annotations = []graph.Annotation{{Site: "svc.Other", Kind: "reflect", Note: "stray", By: "x"}}
	if other, _ := For(graph.NewIndex(g), nil, "svc.Send"); strings.Contains(other.Render(), "stray") {
		t.Error("an annotation for an unrelated site must not appear on this card")
	}
}

// TestAnnotationNotDuplicatedAcrossSameSeam pins the dedup fix: when a site carries
// more than one blind spot of the same kind (a function with two ExternalBoundaryCall
// handoffs to different packages), the shared annotation is collected and rendered
// ONCE, not once per blind spot.
func TestAnnotationNotDuplicatedAcrossSameSeam(t *testing.T) {
	g := &graph.Graph{
		Algo:  "rta",
		Nodes: []graph.Node{{FQN: "svc.Send"}},
		BlindSpots: []graph.BlindSpot{
			{Kind: "ExternalBoundaryCall", Site: "svc.Send", Detail: "hands off to acme"},
			{Kind: "ExternalBoundaryCall", Site: "svc.Send", Detail: "hands off to stripe"},
		},
		Annotations: []graph.Annotation{
			{Site: "svc.Send", Kind: "ExternalBoundaryCall", Note: "the SDK behind this seam POSTs out", By: "dev"},
		},
	}
	card, err := For(graph.NewIndex(g), nil, "svc.Send")
	if err != nil {
		t.Fatal(err)
	}
	if len(card.Annotations) != 1 {
		t.Errorf("annotation collected %d times, want 1: %+v", len(card.Annotations), card.Annotations)
	}
	out := card.Render()
	if n := strings.Count(out, "the SDK behind this seam POSTs out"); n != 1 {
		t.Errorf("annotation rendered %d times, want 1:\n%s", n, out)
	}
	// Both blind-spot rows still appear — dedup is on the annotation, not the spots.
	if n := strings.Count(out, "ExternalBoundaryCall svc.Send"); n != 2 {
		t.Errorf("both blind-spot rows must still render (want 2), got %d:\n%s", n, out)
	}
}

// GX-5 landed criterion: the binding-rules section names exactly the rules
// that demonstrably fire on the function — cross-checked by seeding a
// violation at the function and asserting the named rule catches it.
func TestBindingRulesActuallyBind(t *testing.T) {
	ix := index(t, "layeredsvc.graph.json")
	const sSelectUser = "(*example.com/layeredsvc/internal/store.Store).SelectUser"
	const hGetUserFast = "(*example.com/layeredsvc/internal/handler.Server).GetUserFast"
	p := &policy.Policy{
		Service: "layeredsvc", Version: 1,
		Layers: []policy.Layer{
			{Name: "handler", Packages: []string{"example.com/layeredsvc/internal/handler"}},
			{Name: "app", Packages: []string{"example.com/layeredsvc/internal/app"}},
			{Name: "store", Packages: []string{"example.com/layeredsvc/internal/store"}},
		},
		Layering: &policy.Layering{Roots: []string{"example.com/layeredsvc"}},
		MustPassThrough: []policy.PassRule{{
			Name: "app-guards-db", From: []string{policy.EntrypointSelector},
			To: []string{"boundary:db"}, Through: []string{"(*example.com/layeredsvc/internal/app.Service)"},
		}},
	}

	card, err := For(ix, p, sSelectUser)
	if err != nil {
		t.Fatal(err)
	}
	if card.Layer != "store" {
		t.Errorf("layer = %q, want store", card.Layer)
	}
	var namesLayering bool
	for _, r := range card.Binding {
		if strings.Contains(r, "layering") {
			namesLayering = true
		}
	}
	if !namesLayering {
		t.Fatalf("binding rules %v omit layering", card.Binding)
	}
	// Cross-check: seed the violation the card warns about; the same rule fires.
	g, _ := graph.LoadFile(filepath.Join(goldensDir, "layeredsvc.graph.json"))
	g.Nodes = append(g.Nodes, graph.Node{FQN: hGetUserFast, Sig: "func()", Tier: 1})
	g.Edges = append(g.Edges, graph.Edge{From: hGetUserFast, To: sSelectUser, Tier: 2})
	res := fitness.Check(p, graph.NewIndex(g))
	var fired []string
	for _, f := range res.Violations() {
		fired = append(fired, f.Rule)
	}
	for _, want := range []string{"layering", "must_pass_through"} {
		found := false
		for _, r := range fired {
			if r == want {
				found = true
			}
		}
		if !found {
			t.Errorf("seeded violation did not fire %s (fired: %v)", want, fired)
		}
	}

	// The waypoint card names the must_pass_through rule it implements.
	wcard, err := For(ix, p, "(*example.com/layeredsvc/internal/app.Service).GetProfile")
	if err != nil {
		t.Fatal(err)
	}
	var waypoint bool
	for _, r := range wcard.Binding {
		if strings.Contains(r, "app-guards-db") && strings.Contains(r, "waypoint") {
			waypoint = true
		}
	}
	if !waypoint {
		t.Errorf("waypoint binding missing: %v", wcard.Binding)
	}
}

// Graph-borne facts bind with no policy at all: obligations and partial-effect
// facts ride the graph.
func TestGraphBorneBindingWithoutPolicy(t *testing.T) {
	ix := index(t, "obligsvc.graph.json")
	card, err := For(ix, nil, "example.com/obligsvc/internal/app.Transfer")
	if err != nil {
		t.Fatal(err)
	}
	var oblig bool
	for _, r := range card.Binding {
		if strings.Contains(r, "tx-must-close") && strings.Contains(r, "VIOLATED") {
			oblig = true
		}
	}
	if !oblig {
		t.Fatalf("graph-borne obligation missing from %v", card.Binding)
	}

	dcard, err := For(ix, nil, "example.com/obligsvc/internal/app.DisburseAndCharge")
	if err != nil {
		t.Fatal(err)
	}
	var partial bool
	for _, r := range dcard.Binding {
		if strings.Contains(r, "partial-effect") && strings.Contains(r, "always precedes") {
			partial = true
		}
	}
	if !partial {
		t.Fatalf("partial-effect fact missing from %v", dcard.Binding)
	}
}

// effect_order facts that differ only by call site render to one binding line:
// the card states the rule once, and its "Binding rules (N)" count is the number
// of DISTINCT rules, not the number of underlying facts (F6).
func TestEffectOrderBindingDeduped(t *testing.T) {
	const fn = "example.com/svc/internal/app.Publish"
	g := &graph.Graph{
		Nodes: []graph.Node{{FQN: fn, Sig: "func() error", Tier: 1}},
		// Three publish sites, all preceding the same fallible callee — same
		// rendered fact, three distinct sites in the graph.
		EffectOrder: []graph.EffectOrderFact{
			{Fn: fn, Effect: "boundary:bus PUBLISH <dynamic>", EffectSite: "a.go:10", Callee: "example.com/svc/internal/app.mapErr", CalleeSite: "a.go:20", Always: true},
			{Fn: fn, Effect: "boundary:bus PUBLISH <dynamic>", EffectSite: "a.go:11", Callee: "example.com/svc/internal/app.mapErr", CalleeSite: "a.go:20", Always: true},
			{Fn: fn, Effect: "boundary:bus PUBLISH <dynamic>", EffectSite: "a.go:12", Callee: "example.com/svc/internal/app.mapErr", CalleeSite: "a.go:20", Always: true},
		},
	}
	card, err := For(graph.NewIndex(g), nil, fn)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range card.Binding {
		if strings.Contains(r, "partial-effect") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want one partial-effect binding line, got %d: %v", n, card.Binding)
	}
}

// The ground card's entrypoint-cover claim is defended against the dispatch seam
// that inflates it: a HighFanOut on the BACKWARD reach is disclosed (parity with
// reach/triage, F4) and the cover line is marked over-approximated (F3). The
// earlier forward-only blind-spot scope left this — the line read first — silent.
func TestCoverOverApproxDisclosedAcrossDispatch(t *testing.T) {
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
		BlindSpots: []graph.BlindSpot{{Kind: graph.KindHighFanOut, Site: dispatch, Detail: "resolved to 47 callees"}},
	}
	card, err := For(graph.NewIndex(g), nil, target)
	if err != nil {
		t.Fatal(err)
	}
	if !card.CoverOverApprox {
		t.Fatal("cover crossed a HighFanOut dispatch but CoverOverApprox is false")
	}
	var disclosed bool
	for _, b := range card.BlindSpots {
		if b.Kind == graph.KindHighFanOut {
			disclosed = true
		}
	}
	if !disclosed {
		t.Fatalf("upstream HighFanOut must appear on the card; got %v", card.BlindSpots)
	}
	if !strings.Contains(card.Render(), "over-approx via dispatch") {
		t.Fatalf("cover line must be annotated:\n%s", card.Render())
	}
}

func TestCardDeterministicAndUnknownFQN(t *testing.T) {
	ix := index(t, "obligsvc.graph.json")
	a, _ := For(ix, nil, "example.com/obligsvc/internal/app.Transfer")
	b, _ := For(ix, nil, "example.com/obligsvc/internal/app.Transfer")
	if !reflect.DeepEqual(a, b) {
		t.Fatal("non-deterministic card")
	}
	if _, err := For(ix, nil, "example.com/nope.Missing"); err == nil {
		t.Fatal("unknown FQN must error, not return an empty card")
	}
}
