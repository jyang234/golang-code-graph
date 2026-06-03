package syscontext

import (
	"testing"

	"github.com/jyang234/golang-code-graph/ir"
)

func findEdge(g *Graph, from, to, label string) *Edge {
	for i := range g.Edges {
		e := &g.Edges[i]
		if e.From == from && e.To == to && e.Label == label {
			return e
		}
	}
	return nil
}

func nodeKind(g *Graph, name string) NodeKind {
	for _, n := range g.Nodes {
		if n.Name == name {
			return n.Kind
		}
	}
	return ""
}

// loansvc HTTP→bureau and PUBLISH e1; notifier CONSUME e1 (a second trace).
func sampleTraces() []*ir.CanonicalTrace {
	loan := &ir.CanonicalTrace{Service: "loansvc", Root: &ir.CanonicalSpan{
		Op: "HTTP POST /x", Kind: ir.KindServer, Service: "loansvc",
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
			{Op: "HTTP GET bureau /s", Kind: ir.KindClient, Peer: "bureau", Service: "loansvc"},
			{Op: "PUBLISH e1", Kind: ir.KindProducer, Service: "loansvc"},
		}}},
	}}
	notify := &ir.CanonicalTrace{Service: "notifier", Root: &ir.CanonicalSpan{
		Op: "CONSUME e1", Kind: ir.KindConsumer, Service: "notifier",
	}}
	return []*ir.CanonicalTrace{loan, notify}
}

func TestBuildBusHub(t *testing.T) {
	g := Build(sampleTraces(), nil, Options{})

	if k := nodeKind(g, "Bus"); k != KindBroker {
		t.Errorf("Bus kind = %q, want broker", k)
	}
	if k := nodeKind(g, "loansvc"); k != KindService {
		t.Errorf("loansvc kind = %q, want service", k)
	}
	if k := nodeKind(g, "bureau"); k != KindExternal {
		t.Errorf("bureau kind = %q, want external", k)
	}
	for _, want := range []struct{ from, to, label string }{
		{"Client", "loansvc", ""},
		{"loansvc", "bureau", "HTTP"},
		{"loansvc", "Bus", "publish e1"},
		{"Bus", "notifier", "e1"},
	} {
		e := findEdge(g, want.from, want.to, want.label)
		if e == nil {
			t.Errorf("missing edge %s -%q-> %s", want.from, want.label, want.to)
		} else if e.Dashed {
			t.Errorf("edge %s->%s should be solid (runtime)", want.from, want.to)
		}
	}
}

func TestBuildChoreography(t *testing.T) {
	g := Build(sampleTraces(), nil, Options{Choreography: true})
	if e := findEdge(g, "loansvc", "notifier", "e1"); e == nil || e.Dashed {
		t.Errorf("choreography should join publisher→subscriber on e1 (solid), got %+v", e)
	}
	if nodeKind(g, "Bus") != "" {
		t.Errorf("choreography must not introduce a Bus node")
	}
}

func TestOverlayStaticDashed(t *testing.T) {
	// Runtime exercises e1 + bureau only; the contract also has e2 and a pg dep.
	statics := []Contract{{
		Service:   "loansvc",
		Published: []string{"e1", "e2"},
		Deps:      []Dep{{Peer: "pg", Kind: "http"}},
	}}
	g := Build(sampleTraces(), statics, Options{})

	if e := findEdge(g, "loansvc", "Bus", "publish e1"); e == nil || e.Dashed {
		t.Errorf("e1 is exercised → solid, got %+v", e)
	}
	if e := findEdge(g, "loansvc", "Bus", "publish e2"); e == nil || !e.Dashed {
		t.Errorf("e2 is contract-only → dashed, got %+v", e)
	}
	if e := findEdge(g, "loansvc", "pg", "HTTP"); e == nil || !e.Dashed {
		t.Errorf("pg dep is contract-only → dashed, got %+v", e)
	}
}

// TestServerEdgeFromNesting: when a callee emits a server span whose service.name
// differs from the caller's peer.service, the cross-service hop is recovered from
// the nesting (parent service → this service), not the mismatched client edge.
func TestServerEdgeFromNesting(t *testing.T) {
	tr := &ir.CanonicalTrace{Service: "loansvc", Root: &ir.CanonicalSpan{
		Op: "HTTP POST /x", Kind: ir.KindServer, Service: "loansvc",
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
			{Op: "HTTP GET bureau-host /s", Kind: ir.KindClient, Peer: "bureau-host", Service: "loansvc",
				Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
					{Op: "HTTP GET /s", Kind: ir.KindServer, Service: "bureau"},
				}}}},
		}}},
	}}
	g := Build([]*ir.CanonicalTrace{tr}, nil, Options{})
	if e := findEdge(g, "loansvc", "bureau", ""); e == nil {
		t.Errorf("expected loansvc->bureau recovered from nesting; edges: %+v", g.Edges)
	}
	if nodeKind(g, "bureau") != KindService {
		t.Errorf("the callee server span should be a service node, got %q", nodeKind(g, "bureau"))
	}
}
