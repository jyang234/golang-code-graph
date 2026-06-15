package review

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// The effect contract delta is keyed on the effect TARGET SET (the published
// topic, the outbound endpoint), not the emitting edge. A refactor that moves
// the function emitting an effect — a rename, an extract-function, or a
// consolidation pointing several callers at one helper — leaves the target set
// unchanged and must NOT read as a breaking contract change (the same class as
// R10: keying on node identity, not the contract). A target that genuinely
// leaves the branch entirely still BLOCKs.
func TestContractEffectKeyedOnTargetNotEmitter(t *testing.T) {
	p := &policy.Policy{Service: "svc", Version: 1}
	const topic = "boundary:bus PUBLISH topic.user"
	const endpoint = "boundary:billing POST /charge"

	pub := func(from, to string) graph.Edge {
		return graph.Edge{From: from, To: to, Tier: 1, Boundary: "outbound-sync"}
	}
	mk := func(edges ...graph.Edge) *graph.Graph {
		seen := map[string]bool{}
		var ns []graph.Node
		for _, e := range edges {
			if !seen[e.From] {
				seen[e.From] = true
				ns = append(ns, graph.Node{FQN: e.From, Sig: "func() error", Tier: 1})
			}
		}
		return &graph.Graph{Algo: "vta", Nodes: ns, Edges: edges}
	}

	assertNoBreaking := func(name string, base, branch *graph.Graph) {
		t.Helper()
		a := Review(p, base, branch)
		for _, c := range a.Contract {
			if c.Breaking {
				t.Errorf("%s: moving an emitter with a stable target must not be breaking; got %+v", name, c)
			}
		}
		if a.Verdict == Block {
			t.Errorf("%s: verdict must not be BLOCK; contract=%+v", name, a.Contract)
		}
	}

	// Rename the publisher; the topic is unchanged.
	assertNoBreaking("rename publisher",
		mk(pub("svc.Publisher", topic)),
		mk(pub("svc.PublisherV2", topic)))

	// Extract-function: the old fn now delegates to a new fn that publishes.
	assertNoBreaking("extract-function publisher",
		mk(pub("svc.Publisher", topic)),
		mk(graph.Edge{From: "svc.Publisher", To: "svc.doPublish", Tier: 2}, pub("svc.doPublish", topic)))

	// Rename an outbound caller; the endpoint is unchanged.
	assertNoBreaking("rename outbound caller",
		mk(pub("svc.Caller", endpoint)),
		mk(pub("svc.CallerV2", endpoint)))

	// Consolidation: two functions publish the topic in base; the branch points
	// both at one helper. The topic is still published — not breaking (obligsvc).
	assertNoBreaking("consolidate emitters onto one helper",
		mk(pub("svc.A", topic), pub("svc.B", topic)),
		mk(graph.Edge{From: "svc.A", To: "svc.publish", Tier: 2},
			graph.Edge{From: "svc.B", To: "svc.publish", Tier: 2},
			pub("svc.publish", topic)))

	// Control: the topic genuinely leaves the branch — still a breaking removal.
	bRemoved := Review(p, mk(pub("svc.Publisher", topic)), mk(graph.Edge{From: "svc.Publisher", To: "svc.noop", Tier: 2}))
	var sawBreaking bool
	for _, c := range bRemoved.Contract {
		if c.Op == "-" && c.Surface == "publish" && c.Name == "topic.user" && c.Breaking {
			sawBreaking = true
		}
	}
	if !sawBreaking {
		t.Errorf("a topic that leaves the branch entirely must still be a breaking removal; got %+v", bRemoved.Contract)
	}

	// A genuinely new topic is reported as an addition, never breaking.
	bAdded := Review(p, mk(pub("svc.Publisher", topic)), mk(pub("svc.Publisher", topic), pub("svc.Publisher", "boundary:bus PUBLISH topic.new")))
	var sawAdd bool
	for _, c := range bAdded.Contract {
		if c.Surface == "publish" && c.Name == "topic.new" {
			if c.Op != "+" || c.Breaking {
				t.Errorf("a new topic must be a non-breaking addition; got %+v", c)
			}
			sawAdd = true
		}
	}
	if !sawAdd {
		t.Errorf("a new topic must surface as an added contract effect; got %+v", bAdded.Contract)
	}
}

// A consumed topic is usually both a consumer entrypoint and a `bus CONSUME`
// boundary effect. Removing the consumption must report ONE breaking line (the
// entrypoint surface), not two with the identical Name (entrypoint + consume) —
// the consume effect is deduped against the entrypoint that already represents it.
func TestConsumerTopicNotDoubleReported(t *testing.T) {
	p := &policy.Policy{Service: "svc", Version: 1}
	const topic = "payment.settled"
	base := &graph.Graph{
		Algo:  "vta",
		Nodes: []graph.Node{{FQN: "svc.OnSettled", Sig: "f", Tier: 1}, {FQN: "svc.run", Sig: "f", Tier: 1}},
		Edges: []graph.Edge{
			{From: "svc.run", To: "boundary:bus CONSUME " + topic, Tier: 1, Boundary: "inbound"},
		},
		Entrypoints: []graph.Entrypoint{{Kind: "consumer", Name: topic, Fn: "svc.OnSettled"}},
	}
	branch := &graph.Graph{Algo: "vta", Nodes: []graph.Node{{FQN: "svc.run", Sig: "f", Tier: 1}}}

	a := Review(p, base, branch)
	var named []ContractChange
	for _, c := range a.Contract {
		if c.Name == topic {
			named = append(named, c)
		}
	}
	if len(named) != 1 {
		t.Fatalf("removing a consumed topic must report exactly one contract line for %q; got %+v", topic, named)
	}
	if named[0].Surface != "entrypoint" || !named[0].Breaking || named[0].Op != "-" {
		t.Errorf("the surviving line must be the breaking entrypoint removal; got %+v", named[0])
	}
}

// A mid-flow `bus CONSUME` that is NOT a registered consumer entrypoint must
// still be reported on the consume surface — the dedup only suppresses a consume
// that an entrypoint already represents.
func TestUnregisteredConsumeStillReported(t *testing.T) {
	p := &policy.Policy{Service: "svc", Version: 1}
	base := &graph.Graph{
		Algo:  "vta",
		Nodes: []graph.Node{{FQN: "svc.worker", Sig: "f", Tier: 1}},
		Edges: []graph.Edge{{From: "svc.worker", To: "boundary:bus CONSUME audit.events", Tier: 1, Boundary: "inbound"}},
	}
	branch := &graph.Graph{Algo: "vta", Nodes: []graph.Node{{FQN: "svc.worker", Sig: "f", Tier: 1}}}
	a := Review(p, base, branch)
	var saw bool
	for _, c := range a.Contract {
		if c.Op == "-" && c.Surface == "consume" && c.Name == "audit.events" && c.Breaking {
			saw = true
		}
	}
	if !saw {
		t.Errorf("an unregistered consume removal must still be a breaking contract change; got %+v", a.Contract)
	}
}

// The entrypoint contract delta is keyed on (kind, name): an http route and a
// consumer topic that share a name are distinct contracts, so removing one must
// not be masked by the other surviving. (Name-only keying would collapse them.)
func TestEntrypointKeyedByKindNotJustName(t *testing.T) {
	p := &policy.Policy{Service: "svc", Version: 1}
	const shared = "orders" // an http route and a topic both named "orders"
	base := &graph.Graph{
		Algo:  "vta",
		Nodes: []graph.Node{{FQN: "svc.H", Sig: "f", Tier: 1}, {FQN: "svc.C", Sig: "f", Tier: 1}},
		Entrypoints: []graph.Entrypoint{
			{Kind: "http", Name: shared, Fn: "svc.H"},
			{Kind: "consumer", Name: shared, Fn: "svc.C"},
		},
	}
	// Branch drops only the http route; the consumer topic survives.
	branch := &graph.Graph{
		Algo:        "vta",
		Nodes:       []graph.Node{{FQN: "svc.C", Sig: "f", Tier: 1}},
		Entrypoints: []graph.Entrypoint{{Kind: "consumer", Name: shared, Fn: "svc.C"}},
	}
	a := Review(p, base, branch)
	var sawHTTPRemoval bool
	for _, c := range a.Contract {
		if c.Op == "-" && c.Surface == "entrypoint" && c.Name == shared && c.Breaking {
			sawHTTPRemoval = true
		}
	}
	if !sawHTTPRemoval {
		t.Errorf("removing an http route must be breaking even when a consumer topic of the same name survives; got %+v", a.Contract)
	}
}
