package config

import (
	"strings"
	"testing"
)

// TestResolveAnnotationKind pins the single-source binding rule shared by the
// producer merge (graphio) and the read-only MCP annotate proposer: orphan and
// ambiguity fail closed, a single-kind site adopts its kind, and an error names the
// kinds actually present so a caller can correct without another round-trip.
func TestResolveAnnotationKind(t *testing.T) {
	// Orphan: nothing detected at the site.
	if _, err := ResolveAnnotationKind("svc.Gone", "", nil); err == nil {
		t.Error("a site with no blind spot must be an error")
	}

	// Single kind, requested kind omitted → adopt it (duplicates of one kind are
	// still a single distinct kind, not ambiguous).
	if k, err := ResolveAnnotationKind("svc.Send", "", []string{"ExternalBoundaryCall", "ExternalBoundaryCall"}); err != nil || k != "ExternalBoundaryCall" {
		t.Errorf("single-kind site should adopt; got %q err=%v", k, err)
	}

	// Multiple kinds, omitted → ambiguous, error names both (sorted, deterministic).
	_, err := ResolveAnnotationKind("svc.Mixed", "", []string{"ExternalBoundaryCall", "ConcurrentDispatch"})
	if err == nil {
		t.Fatal("a multi-kind site with no requested kind must be ambiguous")
	}
	if !strings.Contains(err.Error(), "ConcurrentDispatch") || !strings.Contains(err.Error(), "ExternalBoundaryCall") {
		t.Errorf("ambiguity error must name present kinds: %v", err)
	}

	// Named kind present at the site → binds.
	if k, err := ResolveAnnotationKind("svc.Mixed", "ConcurrentDispatch", []string{"ExternalBoundaryCall", "ConcurrentDispatch"}); err != nil || k != "ConcurrentDispatch" {
		t.Errorf("named present kind should bind; got %q err=%v", k, err)
	}

	// Named kind absent at the site → error (the site exists, the kind does not).
	if _, err := ResolveAnnotationKind("svc.Send", "Reflect", []string{"ExternalBoundaryCall"}); err == nil {
		t.Error("a kind absent at the site must be an error")
	}
}
