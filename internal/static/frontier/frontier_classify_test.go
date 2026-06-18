package frontier_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
	"github.com/jyang234/golang-code-graph/internal/static/frontier"
)

// These tests exercise the classifier as the pure function it is: hand-authored
// frontier.Input values, one per marker shape and per NEGATIVE case, asserting the
// exact binning. This is the faithful unit-level complement to the source-fixture
// tests (frontier_test.go), which prove the real analyzer emits the seam shape and
// that graphio.Build embeds the section, but are slow and cannot deterministically
// reach every bin. The boundary labels and `$N`/FQN shapes here mirror what graphio
// emits — verified against the real strictsvc graph by the end-to-end test.

func in(nodes []string, edges [][2]string, entries []frontier.InEntry, blind []frontier.InBlindSpot) *frontier.Input {
	v := &frontier.Input{Nodes: nodes, Entrypoints: entries, BlindSpots: blind}
	for _, e := range edges {
		v.Edges = append(v.Edges, frontier.InEdge{From: e[0], To: e[1]})
	}
	return v
}

func marker(markers []frontier.Marker, kind string) (frontier.Marker, bool) {
	for _, m := range markers {
		if m.Kind == kind {
			return m, true
		}
	}
	return frontier.Marker{}, false
}

// Phase 3: the opaque-db disclosure is fold-aware. Unfolded, it points the reader
// at --reclaim-sql (the B2a builder sub-class folds for free); folded, the marker
// that remains is the genuine B2b residue (the consumer must make it constant), and
// the result/report carry the fold flag so an agent reading the data knows the
// opaque-db count is post-fold.
func TestOpaqueDBDisclosureIsFoldAware(t *testing.T) {
	mk := func(folded bool) *frontier.Input {
		v := in([]string{store},
			[][2]string{{store, "boundary:db ExecContext"}}, nil, nil)
		v.Folded = folded
		return v
	}

	unfolded := frontier.Classify(mk(false))
	if unfolded.Folded {
		t.Error("unfolded result must not be flagged folded")
	}
	m, ok := marker(unfolded.Markers, "opaque-db")
	if !ok || m.Bin != frontier.BinB2 {
		t.Fatalf("want an opaque-db B2 marker, got %+v", unfolded.Markers)
	}
	if !contains(m.ReclaimerHint, "--reclaim-sql") {
		t.Errorf("unfolded hint must point at --reclaim-sql, got %q", m.ReclaimerHint)
	}
	if rep := frontier.Summarize(unfolded, 0); rep.SQLFolded {
		t.Error("unfolded report must not set sql_folded")
	}

	folded := frontier.Classify(mk(true))
	if !folded.Folded {
		t.Error("folded result must be flagged folded")
	}
	fm, _ := marker(folded.Markers, "opaque-db")
	if !contains(fm.ReclaimerHint, "B2b") || !contains(fm.ReclaimerHint, "residue") {
		t.Errorf("folded hint must name the genuine B2b residue, got %q", fm.ReclaimerHint)
	}
	if rep := frontier.Summarize(folded, 0); !rep.SQLFolded {
		t.Error("folded report must set sql_folded so the B2 count reads as the residue")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// FQNs shared across cases.
const (
	wrap  = "(*example.com/svc/internal/api.W).Create"
	clo   = "(*example.com/svc/internal/api.W).Create$1"
	store = "(*example.com/svc/internal/store.S).Del"
)

func TestClassifyMarkerShapes(t *testing.T) {
	cases := []struct {
		name    string
		input   *frontier.Input
		present map[string]frontier.Bin // kind -> expected bin (must appear)
		absent  []string                // kinds that must NOT appear
	}{
		{
			name: "severed closure reaching an effect is B",
			input: in(
				[]string{wrap, clo, store},
				[][2]string{{clo, store}, {store, "boundary:db DELETE provisioning_outbox"}},
				nil, nil),
			present: map[string]frontier.Bin{"severed-closure": frontier.BinB},
		},
		{
			name: "severed closure reaching NO effect is not flagged (benign leaf callback)",
			input: in(
				[]string{"(*example.com/svc/internal/api.W).Cmp", "(*example.com/svc/internal/api.W).Cmp$1"},
				nil, nil, nil),
			absent: []string{"severed-closure"},
		},
		{
			name: "severed closure whose parent is not a node is not flagged",
			input: in(
				[]string{clo, store}, // parent `...Create` absent from nodes
				[][2]string{{clo, store}, {store, "boundary:db DELETE x"}},
				nil, nil),
			absent: []string{"severed-closure"},
		},
		{
			name: "entrypoint severed from its own effect-bearing closure is a starved seam (B)",
			input: in(
				[]string{wrap, clo, store},
				[][2]string{{clo, store}, {store, "boundary:db DELETE x"}},
				[]frontier.InEntry{{Fn: wrap, Name: "POST /x"}}, nil),
			present: map[string]frontier.Bin{"starved-entrypoint": frontier.BinB, "severed-closure": frontier.BinB},
		},
		{
			name: "no-op stub entrypoint owning no effect closure is not starved",
			input: in(
				[]string{"(*example.com/svc/internal/api.W).Health"},
				nil,
				[]frontier.InEntry{{Fn: "(*example.com/svc/internal/api.W).Health", Name: "GET /health"}}, nil),
			absent: []string{"starved-entrypoint"},
		},
		{
			name: "entrypoint reaching an effect directly is not starved",
			input: in(
				[]string{"(*example.com/svc/internal/api.W).List", store},
				[][2]string{{"(*example.com/svc/internal/api.W).List", store}, {store, "boundary:db SELECT users"}},
				[]frontier.InEntry{{Fn: "(*example.com/svc/internal/api.W).List", Name: "GET /list"}}, nil),
			absent: []string{"starved-entrypoint", "opaque-db"},
		},
		{
			name:    "dynamic bus topic is A",
			input:   in([]string{wrap}, [][2]string{{wrap, "boundary:bus PUBLISH <dynamic>"}}, nil, nil),
			present: map[string]frontier.Bin{"dynamic-bus": frontier.BinA},
		},
		{
			name:    "dynamic non-bus effect is A",
			input:   in([]string{wrap}, [][2]string{{wrap, "boundary:<dynamic>"}}, nil, nil),
			present: map[string]frontier.Bin{"dynamic-effect": frontier.BinA},
		},
		{
			name:    "opaque db ExecContext is B2",
			input:   in([]string{wrap}, [][2]string{{wrap, "boundary:db ExecContext"}}, nil, nil),
			present: map[string]frontier.Bin{"opaque-db": frontier.BinB2},
		},
		{
			name:    "opaque db call is B2",
			input:   in([]string{wrap}, [][2]string{{wrap, "boundary:db call"}}, nil, nil),
			present: map[string]frontier.Bin{"opaque-db": frontier.BinB2},
		},
		{
			name: "readable db verbs are not opaque",
			input: in([]string{wrap},
				[][2]string{
					{wrap, "boundary:db DELETE provisioning_outbox"},
					{wrap, "boundary:db SELECT users"},
					{wrap, "boundary:db UPDATE loans"},
				}, nil, nil),
			absent: []string{"opaque-db"},
		},
		{
			name:   "named publish is not dynamic",
			input:  in([]string{wrap}, [][2]string{{wrap, "boundary:bus PUBLISH orders"}}, nil, nil),
			absent: []string{"dynamic-bus", "dynamic-effect"},
		},
		{
			name:    "HighFanOut blind spot is C",
			input:   in([]string{wrap}, nil, nil, []frontier.InBlindSpot{{Kind: string(blindspots.HighFanOut), Site: wrap}}),
			present: map[string]frontier.Bin{string(blindspots.HighFanOut): frontier.BinC},
		},
		{
			name: "reflect/unsafe/cgo/linkname blind spots are A",
			input: in([]string{wrap}, nil, nil, []frontier.InBlindSpot{
				{Kind: string(blindspots.Reflect), Site: wrap},
				{Kind: string(blindspots.Unsafe), Site: "example.com/svc/internal/x"},
				{Kind: string(blindspots.Cgo), Site: "example.com/svc/internal/y"},
				{Kind: string(blindspots.Linkname), Site: "example.com/svc/internal/z"},
			}),
			present: map[string]frontier.Bin{
				string(blindspots.Reflect):  frontier.BinA,
				string(blindspots.Unsafe):   frontier.BinA,
				string(blindspots.Cgo):      frontier.BinA,
				string(blindspots.Linkname): frontier.BinA,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			markers := frontier.Classify(tc.input).Markers
			for kind, bin := range tc.present {
				m, ok := marker(markers, kind)
				if !ok {
					t.Errorf("expected a %q marker; markers = %+v", kind, markers)
					continue
				}
				if m.Bin != bin {
					t.Errorf("%q binned %s, want %s", kind, m.Bin, bin)
				}
			}
			for _, kind := range tc.absent {
				if _, ok := marker(markers, kind); ok {
					t.Errorf("%q must NOT appear; markers = %+v", kind, markers)
				}
			}
		})
	}
}

// The three-valued split: a confirmed-severed route is a marker (not unconfirmed);
// a no-op stub reaching no effect with no severed closure is UNCONFIRMED (not a
// marker, not silently dropped); a route reaching an effect is neither. This is the
// signal that keeps attribution_loss honest as a lower bound.
func TestClassifyUnconfirmedRoutes(t *testing.T) {
	const (
		health = "(*example.com/svc/internal/api.W).Health"
		list   = "(*example.com/svc/internal/api.W).List"
	)
	v := in(
		[]string{wrap, clo, store, health, list},
		[][2]string{
			{clo, store}, {store, "boundary:db DELETE x"}, // wrap: confirmed-severed (own $1)
			{list, "boundary:db SELECT y"}, // list: reaches an effect
			// health: reaches nothing, owns no closure → unconfirmed
		},
		[]frontier.InEntry{
			{Fn: wrap, Name: "POST /x"},
			{Fn: health, Name: "GET /health"},
			{Fn: list, Name: "GET /list"},
		}, nil)
	r := frontier.Classify(v)

	if _, ok := marker(r.Markers, "starved-entrypoint"); !ok {
		t.Error("the confirmed-severed route must be a starved-entrypoint marker")
	}
	if len(r.UnconfirmedRoutes) != 1 || r.UnconfirmedRoutes[0] != health {
		t.Errorf("want exactly the Health stub unconfirmed (not the severed or the effectful route), got %v", r.UnconfirmedRoutes)
	}
}

// The roll-ups (per-bin counts and the two ratios) over a graph mixing one marker
// of each bin, so a miscount in the aggregation is caught independently of the
// per-marker binning above.
func TestSummarizeRollups(t *testing.T) {
	v := in(
		[]string{wrap, clo, store, "(*example.com/svc/internal/api.W).Sync"},
		[][2]string{
			{clo, store},                    // severed closure (B) ...
			{store, "boundary:db DELETE x"}, // ... reaching a classified write
			{"(*example.com/svc/internal/api.W).Sync", "boundary:db ExecContext"},        // opaque write (B2)
			{"(*example.com/svc/internal/api.W).Sync", "boundary:bus PUBLISH <dynamic>"}, // dynamic topic (A)
		},
		[]frontier.InEntry{
			{Fn: wrap, Name: "POST /x"},                                        // severed → starved (B)
			{Fn: "(*example.com/svc/internal/api.W).Sync", Name: "POST /sync"}, // reaches effects → not starved
		},
		[]frontier.InBlindSpot{{Kind: string(blindspots.HighFanOut), Site: wrap}}, // (C)
	)
	r := frontier.Summarize(frontier.Classify(v), 2)

	if r.Counts[frontier.BinA] != 1 || r.Counts[frontier.BinB] != 2 || r.Counts[frontier.BinB2] != 1 || r.Counts[frontier.BinC] != 1 {
		t.Errorf("counts A=%d B=%d B2=%d C=%d, want 1/2/1/1",
			r.Counts[frontier.BinA], r.Counts[frontier.BinB], r.Counts[frontier.BinB2], r.Counts[frontier.BinC])
	}
	if r.Entrypoints != 2 || r.StarvedEntrypoints != 1 || r.AttributionLoss != 0.5 {
		t.Errorf("attribution: %d/%d starved (%.2f), want 1/2 (0.50)",
			r.StarvedEntrypoints, r.Entrypoints, r.AttributionLoss)
	}
	if len(r.Markers) != 5 || r.ReclaimableShare != 2.0/5.0 {
		t.Errorf("markers=%d reclaimable=%.3f, want 5 and 0.400", len(r.Markers), r.ReclaimableShare)
	}
}

// Classification is a pure function of the input (rule R3 / the determinism
// doctrine): the same input yields identical markers.
func TestClassifyDeterministic(t *testing.T) {
	v := in(
		[]string{wrap, clo, store},
		[][2]string{{clo, store}, {store, "boundary:db DELETE x"}},
		[]frontier.InEntry{{Fn: wrap, Name: "POST /x"}}, nil)
	if a, b := frontier.Classify(v), frontier.Classify(v); !reflect.DeepEqual(a, b) {
		t.Errorf("classification not deterministic:\n a=%+v\n b=%+v", a, b)
	}
}

// TestClassifyExcludesImpeachmentSeam pins #13: a ratified ImpeachmentSeam is a
// human-enacted disclosure, not a reclaimable frontier, so it must not enter the
// marker set nor churn ReclaimableShare. Adding a seam alongside a real reclaimable
// marker must leave the marker count and ratios identical to the seam-free input.
func TestClassifyExcludesImpeachmentSeam(t *testing.T) {
	reflectSpot := frontier.InBlindSpot{Kind: string(blindspots.Reflect), Site: wrap}
	seamSpot := frontier.InBlindSpot{Kind: string(blindspots.ImpeachmentSeam), Site: store}

	without := frontier.Summarize(frontier.Classify(in(nil, nil, nil, []frontier.InBlindSpot{reflectSpot})), 0)
	withSeam := frontier.Summarize(frontier.Classify(in(nil, nil, nil, []frontier.InBlindSpot{reflectSpot, seamSpot})), 0)

	if len(withSeam.Markers) != len(without.Markers) {
		t.Fatalf("seam entered the marker set: %d markers with seam vs %d without", len(withSeam.Markers), len(without.Markers))
	}
	if withSeam.ReclaimableShare != without.ReclaimableShare {
		t.Errorf("seam churned ReclaimableShare: %v vs %v", withSeam.ReclaimableShare, without.ReclaimableShare)
	}
	if _, ok := marker(withSeam.Markers, string(blindspots.ImpeachmentSeam)); ok {
		t.Error("ImpeachmentSeam marker present; a ratified seam must not be a frontier marker")
	}
}

// An empty input yields an empty inventory and zero ratios — no divide-by-zero.
func TestClassifyEmpty(t *testing.T) {
	r := frontier.Summarize(frontier.Classify(&frontier.Input{}), 0)
	if len(r.Markers) != 0 || r.ReclaimableShare != 0 || r.AttributionLoss != 0 {
		t.Errorf("empty input must classify to nothing; got %+v", r)
	}
}
