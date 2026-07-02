package reviewtriage

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
)

// M-28: a body change that only REMOVED an outgoing call (e.g. deleting an
// auth-check invocation) leaves the function's signature and surviving edges
// untouched. An add-only changed set would put it in no triage zone, steering the
// reviewer away from the very edit that dropped a guard. changedFns must mark such a
// still-present From as changed.
func TestChangedFnsCatchesLostEdge(t *testing.T) {
	const (
		handler = "example.com/svc/internal/handler.Handle"
		auth    = "example.com/svc/internal/auth.Require"
		store   = "example.com/svc/internal/store.Save"
	)
	base := &graph.Graph{
		Nodes: []graph.Node{{FQN: handler, Sig: "func()"}, {FQN: auth, Sig: "func()"}, {FQN: store, Sig: "func()"}},
		Edges: []graph.Edge{
			{From: handler, To: auth}, // base: handler calls the auth check …
			{From: handler, To: store},
		},
	}
	// Branch: the auth-check call is gone; handler's signature and its store edge
	// are unchanged.
	branch := &graph.Graph{
		Nodes: []graph.Node{{FQN: handler, Sig: "func()"}, {FQN: auth, Sig: "func()"}, {FQN: store, Sig: "func()"}},
		Edges: []graph.Edge{
			{From: handler, To: store},
		},
	}

	got := changedFns(base, branch)
	var found bool
	for _, fn := range got {
		if fn == handler {
			found = true
		}
	}
	if !found {
		t.Errorf("handler removed its auth-check call but is absent from the changed set %v", got)
	}
}

// A base edge whose From was DELETED entirely is not a surviving changed function
// — the deletion is captured elsewhere, and attributing a lost edge to a vanished
// node would inject a non-branch FQN into the triage set.
func TestChangedFnsIgnoresLostEdgeFromDeletedNode(t *testing.T) {
	const (
		gone = "example.com/svc/internal/old.Removed"
		sink = "example.com/svc/internal/store.Save"
	)
	base := &graph.Graph{
		Nodes: []graph.Node{{FQN: gone, Sig: "func()"}, {FQN: sink, Sig: "func()"}},
		Edges: []graph.Edge{{From: gone, To: sink}},
	}
	branch := &graph.Graph{
		Nodes: []graph.Node{{FQN: sink, Sig: "func()"}},
	}
	for _, fn := range changedFns(base, branch) {
		if fn == gone {
			t.Errorf("deleted node %q must not appear in the changed set", gone)
		}
	}
}
