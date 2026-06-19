package graphio

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
)

func annCfg(anns ...config.Annotation) *config.Config {
	c := &config.Config{}
	c.Static.Annotations = anns
	return c
}

// TestMergeAnnotationsBinds covers the happy path: an annotation matching a blind
// spot by (site, kind) binds, an omitted kind adopts a single-kind site's kind,
// and the result is sorted by (Site, Kind).
func TestMergeAnnotationsBinds(t *testing.T) {
	manifest := []blindspots.BlindSpot{
		{Kind: blindspots.ExternalBoundaryCall, Site: "ex.com/svc.Send", Detail: "hands off to acme"},
		{Kind: blindspots.ConcurrentDispatch, Site: "ex.com/svc.Run", Detail: "goroutine"},
	}
	cfg := annCfg(
		config.Annotation{Site: "ex.com/svc.Send", Kind: "ExternalBoundaryCall", Note: "POSTs to acme.example.com", By: "dev@x"},
		config.Annotation{Site: "ex.com/svc.Run", Note: "spawns the dispatch worker"}, // kind omitted, single-kind site
	)
	got, err := mergeAnnotations(manifest, cfg)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 bound annotations, got %d: %+v", len(got), got)
	}
	// Sorted by (Site, Kind): ex.com/svc.Run < ex.com/svc.Send.
	if got[0].Site != "ex.com/svc.Run" || got[0].Kind != "ConcurrentDispatch" {
		t.Errorf("omitted kind should have adopted ConcurrentDispatch; got %+v", got[0])
	}
	if got[1].Site != "ex.com/svc.Send" || got[1].Note != "POSTs to acme.example.com" || got[1].By != "dev@x" {
		t.Errorf("Send annotation bound wrong: %+v", got[1])
	}
}

// TestMergeAnnotationsOrphanFails pins the fail-closed contract: an annotation
// that matches no detected blind spot is refused (a stale FQN is drift), never a
// silent drop.
func TestMergeAnnotationsOrphanFails(t *testing.T) {
	manifest := []blindspots.BlindSpot{{Kind: blindspots.Reflect, Site: "ex.com/svc.Decode", Detail: "reflect"}}
	if _, err := mergeAnnotations(manifest, annCfg(config.Annotation{Site: "ex.com/svc.Gone", Note: "x"})); err == nil {
		t.Fatal("an annotation at a site with no blind spot must be rejected")
	}
	// Site exists but the named kind does not.
	if _, err := mergeAnnotations(manifest, annCfg(config.Annotation{Site: "ex.com/svc.Decode", Kind: "ConcurrentDispatch", Note: "x"})); err == nil {
		t.Fatal("an annotation naming a kind absent at the site must be rejected")
	}
}

// TestMergeAnnotationsAmbiguousKindFails pins that a site carrying more than one
// blind-spot kind requires an explicit kind, so context can never silently attach
// to the wrong shape.
func TestMergeAnnotationsAmbiguousKindFails(t *testing.T) {
	manifest := []blindspots.BlindSpot{
		{Kind: blindspots.ExternalBoundaryCall, Site: "ex.com/svc.Mixed", Detail: "ext"},
		{Kind: blindspots.ConcurrentDispatch, Site: "ex.com/svc.Mixed", Detail: "go"},
	}
	if _, err := mergeAnnotations(manifest, annCfg(config.Annotation{Site: "ex.com/svc.Mixed", Note: "which one?"})); err == nil {
		t.Fatal("an omitted kind on a multi-kind site must be rejected as ambiguous")
	}
	// With the kind given, it binds.
	got, err := mergeAnnotations(manifest, annCfg(config.Annotation{Site: "ex.com/svc.Mixed", Kind: "ConcurrentDispatch", Note: "the worker"}))
	if err != nil || len(got) != 1 || got[0].Kind != "ConcurrentDispatch" {
		t.Fatalf("disambiguated annotation should bind ConcurrentDispatch; got %+v err=%v", got, err)
	}
}

// TestMergeAnnotationsDedupTieBreakIsIntrinsic pins that two annotations colliding
// on (site, kind) collapse to the lexically-smallest note — an intrinsic tie-break,
// not arrival order — so the bound set is byte-identical run-to-run.
func TestMergeAnnotationsDedupTieBreakIsIntrinsic(t *testing.T) {
	manifest := []blindspots.BlindSpot{{Kind: blindspots.Reflect, Site: "ex.com/svc.Decode", Detail: "reflect"}}
	mk := func(n1, n2 string) Annotation {
		got, err := mergeAnnotations(manifest, annCfg(
			config.Annotation{Site: "ex.com/svc.Decode", Kind: "reflect", Note: n1},
			config.Annotation{Site: "ex.com/svc.Decode", Kind: "reflect", Note: n2},
		))
		if err != nil || len(got) != 1 {
			t.Fatalf("collision must collapse to one: %+v err=%v", got, err)
		}
		return got[0]
	}
	if fwd, rev := mk("aaa", "zzz"), mk("zzz", "aaa"); fwd != rev || fwd.Note != "aaa" {
		t.Fatalf("dedup tie-break is arrival-dependent or not lexically-smallest: %+v vs %+v", fwd, rev)
	}

	// EQUAL notes must still tie-break on intrinsic content (By, then Claim), never
	// on config-array position — otherwise the kept By/Claim would depend on file order.
	eq := func(by1, by2 string) Annotation {
		got, err := mergeAnnotations(manifest, annCfg(
			config.Annotation{Site: "ex.com/svc.Decode", Kind: "reflect", Note: "same", By: by1},
			config.Annotation{Site: "ex.com/svc.Decode", Kind: "reflect", Note: "same", By: by2},
		))
		if err != nil || len(got) != 1 {
			t.Fatalf("collision must collapse to one: %+v err=%v", got, err)
		}
		return got[0]
	}
	if fwd, rev := eq("alice", "bob"), eq("bob", "alice"); fwd != rev || fwd.By != "alice" {
		t.Fatalf("equal-note tie-break is arrival-dependent or not lexically-smallest By: %+v vs %+v", fwd, rev)
	}
}

// TestMergeAnnotationsEmptyConfig: no annotations ⇒ nil, never an error.
func TestMergeAnnotationsEmptyConfig(t *testing.T) {
	manifest := []blindspots.BlindSpot{{Kind: blindspots.Reflect, Site: "ex.com/svc.Decode", Detail: "reflect"}}
	got, err := mergeAnnotations(manifest, &config.Config{})
	if err != nil || got != nil {
		t.Fatalf("empty config must yield (nil, nil), got %+v err=%v", got, err)
	}
}
