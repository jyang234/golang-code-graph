package fitness

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// PC-1 rule liveness: a rule whose From binds nothing in the graph is an inert
// guardrail — previously a silent provenAbsent pass, forever. The checks
// disclose it; the Liveness audit lists per-pattern state absolutely (no
// base/branch diff), so born-inert rules stay visible too.

const ghostPkg = "example.com/ghost"

func TestInertMustNotReachFromIsCaution(t *testing.T) {
	p := &policy.Policy{Service: "layeredsvc", Version: 1, MustNotReach: []policy.ReachRule{{
		Name: "ghost-rule", From: []string{ghostPkg}, To: []string{"boundary:db"},
	}}}
	res := Check(p, graph.NewIndex(loadGraph(t, "layeredsvc.graph.json")))

	c := res.Cautions()
	if len(c) != 1 || c[0].Rule != "must_not_reach" || !strings.Contains(c[0].Summary, "binds nothing") {
		t.Fatalf("want one inert-rule caution, got %v", res.Findings)
	}
	if !res.OK() {
		t.Fatal("an inert rule without require_proof must not fail the gate")
	}
}

func TestInertRequireProofIsViolation(t *testing.T) {
	// require_proof means unprovability must not pass CI; a rule that cannot
	// even be evaluated is the strongest form of unprovability.
	p := &policy.Policy{Service: "layeredsvc", Version: 1, MustNotReach: []policy.ReachRule{{
		Name: "ghost-rule", From: []string{ghostPkg}, To: []string{"boundary:db"}, RequireProof: true,
	}}}
	res := Check(p, graph.NewIndex(loadGraph(t, "layeredsvc.graph.json")))

	v := res.Violations()
	if len(v) != 1 || !strings.Contains(v[0].Summary, "inert rule guards nothing") {
		t.Fatalf("want one inert-rule violation, got %v", res.Findings)
	}
}

func TestInertMustPassThroughFrom(t *testing.T) {
	p := passPolicy(policy.PassRule{
		Name: "ghost-guard", From: []string{ghostPkg}, To: []string{"boundary:db"},
		Through: []string{appService},
	})
	res := Check(p, graph.NewIndex(loadGraph(t, "layeredsvc.graph.json")))

	c := res.Cautions()
	if len(c) != 1 || c[0].Rule != "must_pass_through" || !strings.Contains(c[0].Summary, "binds nothing") {
		t.Fatalf("want one inert-rule caution, got %v", res.Findings)
	}
}

// A To that matches nothing is deliberately NOT inert: "the forbidden thing
// does not exist" is the success state for a negative invariant.
// A To that binds nothing in the graph is disclosed, not silently passed
// (corrected after a field run: an unbindable sink — e.g. a third-party
// logger whose methods are not graph nodes — is indistinguishable from "the
// forbidden thing does not exist", so reporting HOLDS would be the silent
// pass the framework exists to prevent). Caution by default; require_proof
// escalates to Violation, exactly like an inert From.
func TestUnbindableToIsDisclosed(t *testing.T) {
	ix := graph.NewIndex(loadGraph(t, "layeredsvc.graph.json"))
	rule := policy.ReachRule{Name: "no-such-target", From: []string{hGetUser}, To: []string{"boundary:peer NOPE"}}

	res := Check(&policy.Policy{Service: "layeredsvc", Version: 1, MustNotReach: []policy.ReachRule{rule}}, ix)
	c := res.Cautions()
	if len(res.Violations()) != 0 || len(c) != 1 || !strings.Contains(c[0].Summary, "to binds nothing") {
		t.Fatalf("an unbindable To must be a disclosed caution, got %v", res.Findings)
	}

	rule.RequireProof = true
	res = Check(&policy.Policy{Service: "layeredsvc", Version: 1, MustNotReach: []policy.ReachRule{rule}}, ix)
	if v := res.Violations(); len(v) != 1 || !strings.Contains(v[0].Summary, "require_proof") {
		t.Fatalf("require_proof must escalate an unbindable To to a violation, got %v", res.Findings)
	}

	// The regression guard: a To that DOES bind (UPDATE exists on UpdateUser)
	// but is unreached from a read-only route stays a real proof — silent
	// success, NOT a caution. The fix must only fire on truly unbindable
	// targets, never on legitimately-unreached ones.
	bound := policy.ReachRule{Name: "reads-no-writes", From: []string{hGetUser}, To: []string{"boundary:db UPDATE"}}
	if r := Check(&policy.Policy{Service: "layeredsvc", Version: 1, MustNotReach: []policy.ReachRule{bound}}, ix); len(r.Findings) != 0 {
		t.Fatalf("a bound-but-unreached To must stay provenAbsent (silent), got %v", r.Findings)
	}
}

func TestLivenessPerPattern(t *testing.T) {
	p := &policy.Policy{
		Service: "layeredsvc", Version: 1,
		MustNotReach: []policy.ReachRule{{
			Name: "mixed",
			From: []string{hGetUser, ghostPkg},
			To:   []string{"boundary:db INSERT", "boundary:nope"},
		}},
		MustPassThrough: []policy.PassRule{{
			Name: "guard", From: []string{policy.EntrypointSelector},
			Through: []string{"example.com/ghost2"}, To: []string{"boundary:db"},
		}},
		NoConcurrentReach: []policy.ConcurrentRule{{
			Name: "sync-writes", To: []string{"boundary:db UPDATE"},
		}},
	}
	ls := Liveness(p, graph.NewIndex(loadGraph(t, "layeredsvc.graph.json")))

	want := map[string]struct{ dead, info bool }{
		"must_not_reach:mixed/from/" + hGetUser:                 {false, false},
		"must_not_reach:mixed/from/" + ghostPkg:                 {true, false},
		"must_not_reach:mixed/to/boundary:db INSERT":            {false, true},
		"must_not_reach:mixed/to/boundary:nope":                 {true, true},
		"must_pass_through:guard/from/entrypoint:*":             {false, false},
		"must_pass_through:guard/through/example.com/ghost2":    {true, false},
		"must_pass_through:guard/to/boundary:db":                {false, true},
		"no_concurrent_reach:sync-writes/to/boundary:db UPDATE": {false, true},
	}
	if len(ls) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(ls), len(want), ls)
	}
	for _, l := range ls {
		key := l.Source + "/" + l.Field + "/" + l.Pattern
		w, ok := want[key]
		if !ok {
			t.Errorf("unexpected entry %s", key)
			continue
		}
		if l.Dead != w.dead || l.Info != w.info {
			t.Errorf("%s: dead=%v info=%v, want dead=%v info=%v", key, l.Dead, l.Info, w.dead, w.info)
		}
	}

	// Only the unambiguous From/Through deaths count; the To-side INFO does not.
	if got := DeadPatternCount(ls); got != 2 {
		t.Errorf("DeadPatternCount = %d, want 2", got)
	}

	// A mixed From list (one live, one dead pattern) is NOT a rule-level inert
	// caution — that escalation is reserved for a whole-From that binds nothing.
	res := Check(p, graph.NewIndex(loadGraph(t, "layeredsvc.graph.json")))
	for _, f := range res.Findings {
		if strings.Contains(f.Summary, "binds nothing") {
			t.Errorf("partially-dead From must be audit-level, not a check finding: %v", f)
		}
	}
}

func TestLivenessStateRendering(t *testing.T) {
	live := PatternLiveness{Source: "must_not_reach:x", Field: "from", Pattern: "a"}
	dead := PatternLiveness{Source: "must_not_reach:x", Field: "from", Pattern: "b", Dead: true}
	info := PatternLiveness{Source: "must_not_reach:x", Field: "to", Pattern: "c", Dead: true, Info: true}
	for s, want := range map[string]string{live.String(): "[LIVE]", dead.String(): "[DEAD]", info.String(): "[INFO]"} {
		if !strings.HasPrefix(s, want) {
			t.Errorf("rendering %q, want prefix %s", s, want)
		}
	}
}
