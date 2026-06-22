package fitness

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/effectkind"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
)

// A method-named outbound effect's (blob/cache/rpc) write-ness is the callee method
// name, which the budget does NOT read as a verb (a method-name heuristic could be
// silently wrong). So IsWrite must return false for it — it is never counted as a
// write — and UnclassifiedEffectLabel must return true, so the budget discloses it
// as an unenforceable write frontier instead of passing it silently.
func TestMethodNamedOutboundWriteness(t *testing.T) {
	cases := []struct {
		to           string
		isWrite      bool
		isUnclassEff bool
	}{
		{"boundary:blob PutObject", false, true},
		{"boundary:blob GetObject", false, true},
		{"boundary:blob Delete", false, true}, // a method NAMED Delete must not be read as an HTTP DELETE write
		{"boundary:cache Set", false, true},
		{"boundary:cache Get", false, true},
		{"boundary:rpc Charge", false, true},
		{"boundary:db INSERT users", true, false},
		{"boundary:bus PUBLISH user.created", true, false},
		{"boundary:checkout POST /charge", true, false}, // HTTP label is "<peer> <METHOD> <route>"
		{"boundary:checkout GET /status", false, false},
	}
	for _, c := range cases {
		e := graph.Edge{To: c.to, Boundary: "outbound-sync"}
		if got := IsWrite(e); got != c.isWrite {
			t.Errorf("IsWrite(%q) = %v, want %v", c.to, got, c.isWrite)
		}
		if _, got := UnclassifiedEffectLabel(e); got != c.isUnclassEff {
			t.Errorf("UnclassifiedEffectLabel(%q) ok = %v, want %v", c.to, got, c.isUnclassEff)
		}
	}
}

// PARITY with the static labeler's kind set: the budget must treat EVERY kind in the
// shared effectkind set as a method-named outbound effect (so adding a kind there can
// never silently drift from the budget). One source of truth, guarded by a test.
func TestBudgetCoversEveryMethodNamedKind(t *testing.T) {
	for _, kind := range effectkind.MethodNamedKinds() {
		e := graph.Edge{To: "boundary:" + kind + " SomeMethod", Boundary: "outbound-sync"}
		if IsWrite(e) {
			t.Errorf("kind %q: IsWrite = true, want false (write-ness must be disclosed, not asserted)", kind)
		}
		if _, ok := UnclassifiedEffectLabel(e); !ok {
			t.Errorf("kind %q: UnclassifiedEffectLabel ok = false, want true (must be disclosed as unenforceable)", kind)
		}
	}
}

// An HTTP peer literally NAMED like a kind ("boundary:cache POST /x", three fields)
// must not collide with the kind set: it is a real HTTP write, not a method-named
// effect. The two-field shape is what disambiguates.
func TestHTTPPeerNamedLikeKindIsNotMethodNamed(t *testing.T) {
	for _, kind := range effectkind.MethodNamedKinds() {
		e := graph.Edge{To: "boundary:" + kind + " POST /charge", Boundary: "outbound-sync"}
		if !IsWrite(e) {
			t.Errorf("HTTP peer %q with POST must be a write, not dropped as a method-named effect", kind)
		}
		if _, ok := UnclassifiedEffectLabel(e); ok {
			t.Errorf("HTTP peer %q (3-field label) must not be mis-tagged as an unclassified method-named effect", kind)
		}
	}
}
