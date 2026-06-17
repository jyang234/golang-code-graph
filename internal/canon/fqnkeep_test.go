package canon

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/capture"
	"github.com/jyang234/golang-code-graph/ir"
)

// taggedWaypointFlow is a flow with TWO internal-compute wrappers, each wrapping a
// distinct DB effect: one carries an L1 flowmap.fqn tag (a producer-tagged
// first-party waypoint), the other does not. Tier-3 internal compute is normally
// contracted away; the keep-rule must preserve ONLY the tagged one (plan §7).
func taggedWaypointFlow() capture.CapturedFlow {
	spans := []capture.Span{
		{ID: "root", Kind: ir.KindServer, Status: capture.StatusOK, Start: ms(0, 0), End: ms(0, 100),
			Attrs: map[string]string{"http.request.method": "POST", "http.route": "/x", capture.CorrelationKey: "run"}},

		{ID: "tagged", ParentID: "root", Kind: ir.KindInternal, Name: "taggedWaypoint", Start: ms(0, 1), End: ms(0, 20),
			Attrs: map[string]string{capture.FQNTagKey: "example.com/svc/internal/admin.(*Admin).Purge", capture.CorrelationKey: "run"}},
		{ID: "del", ParentID: "tagged", Kind: ir.KindClient, Start: ms(0, 2), End: ms(0, 10),
			Attrs: map[string]string{"db.system": "postgres", "db.statement": "DELETE FROM ledger WHERE id = 1", capture.CorrelationKey: "run"}},

		{ID: "untagged", ParentID: "root", Kind: ir.KindInternal, Name: "untaggedCompute", Start: ms(0, 30), End: ms(0, 50),
			Attrs: map[string]string{capture.CorrelationKey: "run"}},
		{ID: "ins", ParentID: "untagged", Kind: ir.KindClient, Start: ms(0, 31), End: ms(0, 40),
			Attrs: map[string]string{"db.system": "postgres", "db.statement": "INSERT INTO audit (id) VALUES (1)", capture.CorrelationKey: "run"}},
	}
	return capture.CapturedFlow{Flow: "POST /x", Service: "svc", Spans: spans, Root: &spans[0], Complete: true}
}

func allOps(s *ir.CanonicalSpan, out *[]string) {
	*out = append(*out, s.Op)
	for _, g := range s.Children {
		for _, m := range g.Members {
			allOps(m, out)
		}
	}
}

// fqnOf finds the canonical span with the given op and returns its flowmap.fqn.
func fqnOf(s *ir.CanonicalSpan, op string) (string, bool) {
	if s.Op == op {
		return s.Attrs[capture.FQNTagKey], true
	}
	for _, g := range s.Children {
		for _, m := range g.Members {
			if v, ok := fqnOf(m, op); ok {
				return v, true
			}
		}
	}
	return "", false
}

// TestFQNTaggedInternalKeptUntaggedDropped is the scoped keep-rule (plan §7): a
// tier-3 internal span is PRESERVED when it carries a flowmap.fqn waypoint tag and
// DROPPED when it does not — so the localization signal survives while ordinary
// compute is still contracted away (the keep never broadly stops pruning).
func TestFQNTaggedInternalKeptUntaggedDropped(t *testing.T) {
	tr := mustCanon(t, taggedWaypointFlow())
	var ops []string
	allOps(tr.Root, &ops)
	joined := strings.Join(ops, "\n")

	if !strings.Contains(joined, "taggedWaypoint") {
		t.Errorf("tagged internal waypoint was dropped; ops:\n%s", joined)
	}
	if strings.Contains(joined, "untaggedCompute") {
		t.Errorf("untagged internal compute was NOT dropped (keep-rule too broad); ops:\n%s", joined)
	}
	// The tag survives canon's attribute projection, verbatim — the severance walk
	// reconciles exactly this runtime spelling to an ssa node.
	if got, _ := fqnOf(tr.Root, "taggedWaypoint"); got != "example.com/svc/internal/admin.(*Admin).Purge" {
		t.Errorf("kept waypoint's flowmap.fqn = %q, want the producer's runtime FQN", got)
	}
	// Both DB effects survive (the tagged one nested under its waypoint, the
	// untagged one promoted to the root) — keeping a waypoint never loses an effect.
	if !strings.Contains(joined, "DB postgres DELETE ledger") || !strings.Contains(joined, "DB postgres INSERT audit") {
		t.Errorf("an effect was lost by the keep/contract; ops:\n%s", joined)
	}
}

// TestFQNEmptyTagDoesNotKeep isolates the keep predicate on a NON-EMPTY tag: a
// tier-3 internal span carrying an explicit empty flowmap.fqn ("" — the producer's
// fail-closed ⊥ when no first-party opener was found) must still be DROPPED. The
// keep is `Attrs[FQNTagKey] != ""`, not `_, ok := Attrs[FQNTagKey]`, so an honest ⊥
// can never accidentally preserve a compute span the localization cannot anchor on.
func TestFQNEmptyTagDoesNotKeep(t *testing.T) {
	cf := taggedWaypointFlow()
	// Re-tag the "untagged" compute with an EXPLICIT empty FQN (present key, "" value).
	for i := range cf.Spans {
		if cf.Spans[i].ID == "untagged" {
			cf.Spans[i].Attrs[capture.FQNTagKey] = ""
		}
	}
	fixupRoot(&cf)
	tr := mustCanon(t, cf)
	var ops []string
	allOps(tr.Root, &ops)
	joined := strings.Join(ops, "\n")
	if strings.Contains(joined, "untaggedCompute") {
		t.Errorf("a span with an empty flowmap.fqn (the ⊥) was kept; the keep must require a non-empty tag; ops:\n%s", joined)
	}
	// Its effect is still preserved (promoted to the root), exactly as for a span
	// with no tag key at all.
	if !strings.Contains(joined, "DB postgres INSERT audit") {
		t.Errorf("dropping the empty-tagged waypoint lost its effect; ops:\n%s", joined)
	}
}

// twoTaggedWaypointFlow has TWO tagged tier-3 waypoints whose canonical op is
// IDENTICAL ("evaluate"), each wrapping a distinct DB effect and carrying a distinct
// flowmap.fqn. Their intervals OVERLAP (wpA [1,40], wpB [10,50]) with no goroutine
// signal, so capture.Concurrent falls back to interval overlap and joins them into a
// single CONCURRENT group. Within that group they tie on the canonical op key
// ("evaluate"), forcing canon's bySig tie-break to the canonical subtree signature —
// the content-intrinsic ordering a sequential (non-overlapping) pair would never
// reach (it would sort by start time instead).
func twoTaggedWaypointFlow() capture.CapturedFlow {
	spans := []capture.Span{
		{ID: "root", Kind: ir.KindServer, Status: capture.StatusOK, Start: ms(0, 0), End: ms(0, 100),
			Attrs: map[string]string{"http.request.method": "POST", "http.route": "/x", capture.CorrelationKey: "run"}},

		{ID: "wpA", ParentID: "root", Kind: ir.KindInternal, Name: "evaluate", Start: ms(0, 1), End: ms(0, 40),
			Attrs: map[string]string{capture.FQNTagKey: "example.com/svc/internal/a.(*A).Eval", capture.CorrelationKey: "run"}},
		{ID: "delA", ParentID: "wpA", Kind: ir.KindClient, Start: ms(0, 2), End: ms(0, 10),
			Attrs: map[string]string{"db.system": "postgres", "db.statement": "DELETE FROM ledger WHERE id = 1", capture.CorrelationKey: "run"}},

		{ID: "wpB", ParentID: "root", Kind: ir.KindInternal, Name: "evaluate", Start: ms(0, 10), End: ms(0, 50),
			Attrs: map[string]string{capture.FQNTagKey: "example.com/svc/internal/b.(*B).Eval", capture.CorrelationKey: "run"}},
		{ID: "insB", ParentID: "wpB", Kind: ir.KindClient, Start: ms(0, 12), End: ms(0, 40),
			Attrs: map[string]string{"db.system": "postgres", "db.statement": "INSERT INTO audit (id) VALUES (1)", capture.CorrelationKey: "run"}},
	}
	return capture.CapturedFlow{Flow: "POST /x", Service: "svc", Spans: spans, Root: &spans[0], Complete: true}
}

// TestFQNTwoTaggedWaypointsTieDeterministic pins the tie case: two kept waypoints in
// one concurrent group that tie on the canonical op key must (a) both survive the
// keep, (b) be grouped CONCURRENT (so the bySig path is actually exercised, not the
// sequential start-time sort), and (c) order byte-identically regardless of input
// arrival order — the tie-break resolves on intrinsic content (the subtree
// signature), never on arrival (CLAUDE.md determinism).
func TestFQNTwoTaggedWaypointsTieDeterministic(t *testing.T) {
	tr := mustCanon(t, twoTaggedWaypointFlow())
	var ops []string
	allOps(tr.Root, &ops)
	joined := strings.Join(ops, "\n")
	// Both effects (hence both waypoints' subtrees) survive.
	if !strings.Contains(joined, "DB postgres DELETE ledger") || !strings.Contains(joined, "DB postgres INSERT audit") {
		t.Fatalf("a tied tagged waypoint's effect was lost; ops:\n%s", joined)
	}
	// The two op-key-tied waypoints must land in ONE concurrent group, or the
	// signature tie-break is never reached and this test would pass for the wrong
	// reason (sequential start-time ordering).
	var concurrentEvals bool
	for _, g := range tr.Root.Children {
		evals := 0
		for _, m := range g.Members {
			if m.Op == "evaluate" {
				evals++
			}
		}
		if g.Concurrent && evals == 2 {
			concurrentEvals = true
		}
	}
	if !concurrentEvals {
		t.Fatalf("the two 'evaluate' waypoints did not form a concurrent group, so bySig was not exercised; tree:\n%s", marshal(t, tr))
	}
	want := marshal(t, tr)
	for i := 0; i < 8; i++ {
		cf := twoTaggedWaypointFlow()
		shuffleSpans(cf.Spans)
		fixupRoot(&cf)
		if got := marshal(t, mustCanon(t, cf)); string(got) != string(want) {
			t.Fatalf("tied tagged waypoints ordered non-deterministically under shuffle:\n--- want ---\n%s\n--- got ---\n%s", want, got)
		}
	}
}

// TestFQNTaggedInternalDeterministic confirms the kept waypoint does not perturb
// determinism: shuffling the input span order yields byte-identical IR (the
// snapshot-gate guarantee, extended to the newly-kept spans).
func TestFQNTaggedInternalDeterministic(t *testing.T) {
	want := marshal(t, mustCanon(t, taggedWaypointFlow()))
	for i := 0; i < 8; i++ {
		cf := taggedWaypointFlow()
		shuffleSpans(cf.Spans)
		fixupRoot(&cf)
		if got := marshal(t, mustCanon(t, cf)); string(got) != string(want) {
			t.Fatalf("kept waypoint perturbed determinism under shuffle:\n--- want ---\n%s\n--- got ---\n%s", want, got)
		}
	}
}
