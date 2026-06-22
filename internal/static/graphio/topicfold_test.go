package graphio_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
)

func topicFixture(t *testing.T) *analyze.Result {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", "topicfoldsvc")
	res, err := analyze.Analyze(dir)
	if err != nil {
		t.Fatalf("analyze topicfoldsvc: %v", err)
	}
	return res
}

// busLabels maps each "bus …" boundary label to its via provenance.
func busLabels(g *graphio.Graph) map[string]string {
	out := map[string]string{}
	for _, e := range g.Edges {
		if label, ok := strings.CutPrefix(e.To, "boundary:bus "); ok {
			out[label] = e.Via
		}
	}
	return out
}

// Without the opt-in, a non-constant topic stays <dynamic> and no edge carries a fold
// tag — the default build is byte-for-byte unchanged (D2).
func TestTopicFoldOffByDefault(t *testing.T) {
	g, err := graphio.Build(topicFixture(t), "")
	if err != nil {
		t.Fatal(err)
	}
	labels := busLabels(g)
	if _, ok := labels["PUBLISH <dynamic>"]; !ok {
		t.Errorf("without --reclaim-topic the Phi topic should be <dynamic>; got %v", labels)
	}
	if _, ok := labels["PUBLISH orders.created"]; !ok {
		t.Errorf("the constant topic should always resolve; got %v", labels)
	}
	for label, via := range labels {
		if via != "" {
			t.Errorf("default build must carry no fold provenance; %q has via=%q", label, via)
		}
	}
}

// With --reclaim-topic the fold names the finite constant set (one edge per topic,
// tagged via=topic-constfold), leaves the constant topic untagged, and leaves the
// genuinely-dynamic topic <dynamic> — the whole trichotomy through the labeler.
func TestTopicFoldRecoversConstSet(t *testing.T) {
	g, err := graphio.Build(topicFixture(t), "", graphio.WithTopicFold())
	if err != nil {
		t.Fatal(err)
	}
	labels := busLabels(g)

	for _, want := range []string{"PUBLISH orders.shipped", "PUBLISH orders.cancelled"} {
		via, ok := labels[want]
		if !ok {
			t.Errorf("reclaim-topic should recover %q; got %v", want, labels)
			continue
		}
		if via != "topic-constfold" {
			t.Errorf("%q via = %q, want topic-constfold", want, via)
		}
	}
	// The constant topic is resolved by the labeler, not the fold — no provenance.
	if via, ok := labels["PUBLISH orders.created"]; !ok || via != "" {
		t.Errorf("constant topic must resolve with no fold provenance, got via=%q ok=%v", via, ok)
	}
	// A genuinely dynamic topic still abstains soundly.
	if _, ok := labels["PUBLISH <dynamic>"]; !ok {
		t.Errorf("the os.Getenv topic must stay <dynamic>; got %v", labels)
	}
}
