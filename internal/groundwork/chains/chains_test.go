package chains

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// producerGraph: one function that publishes order.placed, with an in-frame
// effect_order fact (the publish certainly precedes a fallible charge) and a
// SATISFIED must-precede verdict.
func producerGraph() *graph.Graph {
	pub := "(*orders/app.Svc).Place"
	return &graph.Graph{
		Nodes: []graph.Node{{FQN: pub}},
		Edges: []graph.Edge{
			{From: pub, To: "boundary:bus PUBLISH order.placed", Boundary: "outbound-async"},
		},
		EffectOrder: []graph.EffectOrderFact{
			{Fn: pub, Effect: "boundary:bus PUBLISH order.placed", Callee: "billing.Charge", Always: true},
		},
		Obligations: []graph.Obligation{
			{Rule: "audit-before-publish", Kind: "must-precede", Fn: pub, Status: "SATISFIED"},
		},
	}
}

// consumerGraph: a consumer entrypoint on order.placed whose handler commits a
// DB write (and an inbound consume edge that must NOT show as a committed effect).
func consumerGraph() *graph.Graph {
	h := "(*fulfil/app.Svc).OnPlaced"
	return &graph.Graph{
		Nodes: []graph.Node{{FQN: h}},
		Edges: []graph.Edge{
			{From: h, To: "boundary:bus CONSUME order.placed", Boundary: "inbound"},
			{From: h, To: "boundary:db INSERT shipments", Boundary: "outbound-sync"},
		},
		Entrypoints: []graph.Entrypoint{{Kind: "consumer", Name: "order.placed", Fn: h}},
	}
}

func unsignedBus() map[string]policy.Broker {
	return map[string]policy.Broker{"bus": {
		Transport: "sns->sqs", Delivery: "at-least-once", Ordered: "false", Consumers: "idempotent",
	}}
}

func TestBuildCompleteChain(t *testing.T) {
	fleet := []Service{
		{Name: "orders", Index: graph.NewIndex(producerGraph())},
		{Name: "fulfil", Index: graph.NewIndex(consumerGraph())},
	}
	r := Build(fleet, unsignedBus())
	if len(r.Cards) != 1 {
		t.Fatalf("want 1 card, got %d", len(r.Cards))
	}
	c := r.Cards[0]
	if c.Event != "order.placed" {
		t.Fatalf("event = %q", c.Event)
	}
	if c.Open != "" {
		t.Errorf("a complete chain must not be open, got %q", c.Open)
	}
	if len(c.Producers) != 1 || c.Producers[0].Service != "orders" {
		t.Fatalf("producers = %+v", c.Producers)
	}
	if !hasFactContaining(c.Producers[0].Facts, "CERTAINLY committed before fallible billing.Charge") {
		t.Errorf("missing in-frame producer ordering fact: %v", c.Producers[0].Facts)
	}
	if !hasFactContaining(c.Producers[0].Facts, "audit-before-publish: SATISFIED") {
		t.Errorf("missing must-precede verdict: %v", c.Producers[0].Facts)
	}
	if len(c.Consumers) != 1 || c.Consumers[0].Service != "fulfil" {
		t.Fatalf("consumers = %+v", c.Consumers)
	}
	if !hasFactContaining(c.Consumers[0].Facts, "commits db INSERT shipments") {
		t.Errorf("missing consumer downstream effect: %v", c.Consumers[0].Facts)
	}
	if hasFactContaining(c.Consumers[0].Facts, "CONSUME order.placed") {
		t.Errorf("the inbound consume must not be listed as a committed effect: %v", c.Consumers[0].Facts)
	}
	// Broker link is assumed and, here, unsigned — the values present but no warrant.
	if c.Broker.Undeclared || c.Broker.Name != "bus" || c.Broker.Signed {
		t.Errorf("broker link = %+v; want declared, named bus, unsigned", c.Broker)
	}
	out := c.Render()
	for _, want := range []string{"[proven] producer", "[assumed] broker", "UNSIGNED", "[proven] consumer"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered card missing %q:\n%s", want, out)
		}
	}
}

func TestBuildSignedBroker(t *testing.T) {
	brokers := unsignedBus()
	b := brokers["bus"]
	b.SignedBy = "John, 2026-06-13"
	brokers["bus"] = b
	r := Build([]Service{{Name: "orders", Index: graph.NewIndex(producerGraph())}}, brokers)
	link := r.Cards[0].Broker
	if !link.Signed {
		t.Fatalf("broker should read as signed: %+v", link)
	}
	if out := r.Cards[0].Render(); !strings.Contains(out, "signed by John") || strings.Contains(out, "UNSIGNED") {
		t.Errorf("signed broker should print its warrant, not UNSIGNED:\n%s", out)
	}
}

func TestBuildOpenChains(t *testing.T) {
	// Producer with no consumer in the fleet → open downstream.
	r := Build([]Service{{Name: "orders", Index: graph.NewIndex(producerGraph())}}, nil)
	if !strings.Contains(r.Cards[0].Open, "open downstream") {
		t.Errorf("producer-only chain should be open downstream: %q", r.Cards[0].Open)
	}
	// An undeclared broker is disclosed, not silently assumed.
	if out := r.Cards[0].Render(); !strings.Contains(out, "undeclared") {
		t.Errorf("no broker block should render as undeclared:\n%s", out)
	}
	// Consumer with no producer → open upstream.
	r = Build([]Service{{Name: "fulfil", Index: graph.NewIndex(consumerGraph())}}, nil)
	if !strings.Contains(r.Cards[0].Open, "open upstream") {
		t.Errorf("consumer-only chain should be open upstream: %q", r.Cards[0].Open)
	}
}

func TestBuildBrokerSelection(t *testing.T) {
	g := []Service{{Name: "orders", Index: graph.NewIndex(producerGraph())}}

	// A single non-"bus" broker is still printed (the sole guarantee).
	r := Build(g, map[string]policy.Broker{"kafka": {Delivery: "at-least-once"}})
	if l := r.Cards[0].Broker; l.Undeclared || l.Name != "kafka" {
		t.Errorf("a lone broker should be selected by name: %+v", l)
	}

	// Several brokers and none named "bus": decline, but disclose the names
	// rather than read as if nothing were configured.
	r = Build(g, map[string]policy.Broker{
		"kafka": {Delivery: "at-least-once"},
		"sqs":   {Delivery: "at-most-once"},
	})
	l := r.Cards[0].Broker
	if !l.Undeclared {
		t.Fatalf("ambiguous broker set should not be silently selected: %+v", l)
	}
	out := r.Cards[0].Render()
	for _, want := range []string{"kafka", "sqs", `none named "bus"`} {
		if !strings.Contains(out, want) {
			t.Errorf("unselected brokers must be disclosed (%q):\n%s", want, out)
		}
	}
	if strings.Contains(out, "undeclared:") {
		t.Errorf("declared-but-unselected must not read as undeclared:\n%s", out)
	}
}

func TestBuildNoEvents(t *testing.T) {
	r := Build([]Service{{Name: "empty", Index: graph.NewIndex(&graph.Graph{Nodes: []graph.Node{}})}}, nil)
	if len(r.Cards) != 0 {
		t.Fatalf("an eventless fleet yields no cards, got %d", len(r.Cards))
	}
	if !strings.Contains(r.Render(), "no bus events") {
		t.Errorf("empty report should say so: %q", r.Render())
	}
}

func hasFactContaining(facts []string, sub string) bool {
	for _, f := range facts {
		if strings.Contains(f, sub) {
			return true
		}
	}
	return false
}
