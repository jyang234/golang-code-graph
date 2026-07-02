package canon

import (
	"fmt"
	"testing"

	"github.com/jyang234/golang-code-graph/capture"
	"github.com/jyang234/golang-code-graph/ir"
)

// FuzzCanonConcurrentOrderInvariant explores the determinism contract beyond the
// hand-built fixtures (TestDeterminism): within a CONCURRENT group, "a race must
// never perturb the snapshot" (canon §3.3). The concurrency here is driven purely
// by the goroutine signal (every span on its own goroutine, so every sibling pair
// is async ⇒ concurrent regardless of timing), which lets the fuzzer permute the
// siblings' start times WITHOUT changing the concurrency structure. The only thing
// that may differ between the two canonicalizations is which sibling "started
// first" — a run-dependent race the IR must be blind to. Any byte difference is a
// real determinism regression — the class the same-op concurrent-sibling ordering
// bug belonged to (which sorts by Op alone and falls back to start order on a
// tie). The invariant is self-checking: there is no oracle or judge.
//
// The PR gate replays the seed corpus under `go test`; the nightly fuzz job
// explores well past the seeds.
func FuzzCanonConcurrentOrderInvariant(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	f.Add([]byte{0, 0, 9, 9, 3, 3, 7, 7, 5, 5, 1, 1})
	f.Add([]byte{4, 0, 0, 0, 0, 4, 0, 0, 0, 0, 4, 0}) // same-op siblings, distinct subtrees
	f.Fuzz(func(t *testing.T, in []byte) {
		ordered := genFlow(in, false)
		want, err := Canonicalize(ordered, nil)
		if err != nil {
			return // not a valid, complete capture — nothing to compare
		}
		// Same flow, same concurrency structure, only the siblings' start times
		// permuted among themselves: the snapshot must not move.
		raced := genFlow(in, true)
		got, err := Canonicalize(raced, nil)
		if err != nil {
			t.Fatalf("permuting concurrent start times flipped canon to error: %v", err)
		}
		if w, g := marshal(t, want), marshal(t, got); string(w) != string(g) {
			t.Fatalf("concurrent IR depends on sibling start order (a race perturbed the snapshot):\n--- want ---\n%s\n--- raced ---\n%s", w, g)
		}
	})
}

// FuzzCanonSiblingOrderInvariant explores the M-3 determinism contract: siblings on
// a SHARED or ZERO goroutine with coarse (frequently-equal) caller-clock starts
// carry no reliable happens-before, so their canonical order must be a function of
// intrinsic content, never the run-random span id or the arrival order. The fuzzer
// canonicalizes a flat sibling set, then re-canonicalizes the SAME siblings with
// their arrival order reversed and their span ids relabeled — exactly what a re-run
// with fresh random ids does — and requires byte-identical output. Before the fix,
// an equal-start tie fell through to `ordered[i].ID < ordered[j].ID`, so this would
// diverge. Self-checking: no oracle.
func FuzzCanonSiblingOrderInvariant(f *testing.F) {
	f.Add([]byte{2, 5, 5, 5, 5, 5})
	f.Add([]byte{4, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{6, 1, 1, 2, 2, 1, 1, 3, 3})
	f.Fuzz(func(t *testing.T, in []byte) {
		want, err := Canonicalize(genTiedFlow(in, false), nil)
		if err != nil {
			return
		}
		got, err := Canonicalize(genTiedFlow(in, true), nil)
		if err != nil {
			t.Fatalf("relabeling sibling ids/order flipped canon to error: %v", err)
		}
		if w, g := marshal(t, want), marshal(t, got); string(w) != string(g) {
			t.Fatalf("sibling IR depends on span id / arrival order:\n--- want ---\n%s\n--- relabeled ---\n%s", w, g)
		}
	})
}

// genTiedFlow builds a flat set of root children on goroutine 0 (no concurrency
// signal) with coarse start times drawn from a tiny range so equal-start ties are
// common. When relabel is set the children are emitted in reverse arrival order with
// their span ids renamed, isolating the "does span id / arrival order reach output"
// question. Children never parent each other, so relabeling is a pure permutation.
func genTiedFlow(in []byte, relabel bool) capture.CapturedFlow {
	r := &byteReader{data: in}
	n := int(r.b()%8) + 1 // 1..8 children
	type kid struct {
		start, dur int
		kind       ir.Kind
		attrs      map[string]string
	}
	kids := make([]kid, n)
	for i := 0; i < n; i++ {
		k := kid{
			start: int(r.b()) % 4, // tiny range ⇒ frequent equal starts
			dur:   int(r.b()) % 2, // 0 or 1 ms ⇒ often zero-duration
		}
		switch r.b() % 3 {
		case 0:
			k.kind, k.attrs = ir.KindClient, map[string]string{"db.system": "postgres", "db.statement": fuzzDBStmts[int(r.b())%len(fuzzDBStmts)]}
		case 1:
			k.kind, k.attrs = ir.KindProducer, map[string]string{"messaging.destination.name": fuzzTopics[int(r.b())%len(fuzzTopics)]}
		default:
			k.kind, k.attrs = ir.KindClient, map[string]string{"http.request.method": "GET", "peer.service": fuzzPeers[int(r.b())%len(fuzzPeers)], "http.target": "/t"}
		}
		kids[i] = k
	}
	spans := []capture.Span{{
		ID: "root", Kind: ir.KindServer, Status: capture.StatusOK,
		Start: ms(0, 0), End: ms(0, 1000), Goroutine: 1,
		Attrs: map[string]string{"http.request.method": "POST", "http.route": "/x"},
	}}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	if relabel {
		for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
			order[i], order[j] = order[j], order[i]
		}
	}
	for emit, i := range order {
		k := kids[i]
		id := fmt.Sprintf("s%d", i)
		if relabel {
			id = fmt.Sprintf("r%d", emit) // fresh, differently-ordered ids
		}
		spans = append(spans, capture.Span{
			ID: id, ParentID: "root", Kind: k.kind, Goroutine: 0,
			Start: ms(0, 1+k.start), End: ms(0, 1+k.start+k.dur), Attrs: k.attrs,
		})
	}
	return capture.CapturedFlow{Flow: "f", Service: "s", Spans: spans, Root: &spans[0], Complete: true}
}

// genFlow is a total, pure generator: it maps arbitrary fuzz bytes to a valid,
// Complete single-root flow so almost every input exercises canon rather than
// bouncing off the incomplete-capture guard. Every span is placed on its OWN
// goroutine, so every parent→child and sibling↔sibling pair is asynchronous and
// the concurrency grouping is fixed by structure, not timing. When permuteStarts
// is set the children's start times are reversed among themselves — the same race
// outcome a re-run might observe — leaving the concurrency structure untouched.
func genFlow(in []byte, permuteStarts bool) capture.CapturedFlow {
	r := &byteReader{data: in}
	spans := []capture.Span{{
		ID: "root", Kind: ir.KindServer, Status: capture.StatusOK,
		Start: ms(0, 0), End: ms(0, 1000),
		Goroutine: 1,
		Attrs:     map[string]string{"http.request.method": "POST", "http.route": "/x"},
	}}
	n := int(r.b()%10) + 1 // 1..10 children
	starts := make([]int, n)
	durs := make([]int, n)
	for i := 0; i < n; i++ {
		parent := "root"
		if i > 0 && r.b()%2 == 0 {
			parent = fmt.Sprintf("s%d", int(r.b())%i)
		}
		starts[i] = int(r.b()) % 200
		durs[i] = int(r.b())%50 + 10
		var (
			kind  ir.Kind
			attrs map[string]string
		)
		switch r.b() % 3 {
		case 0:
			kind, attrs = ir.KindClient, map[string]string{"db.system": "postgres", "db.statement": fuzzDBStmts[int(r.b())%len(fuzzDBStmts)]}
		case 1:
			kind, attrs = ir.KindProducer, map[string]string{"messaging.destination.name": fuzzTopics[int(r.b())%len(fuzzTopics)]}
		default:
			kind, attrs = ir.KindClient, map[string]string{"http.request.method": "GET", "peer.service": fuzzPeers[int(r.b())%len(fuzzPeers)], "http.target": "/t"}
		}
		spans = append(spans, capture.Span{
			ID: fmt.Sprintf("s%d", i), ParentID: parent, Kind: kind,
			Goroutine: uint64(i + 2), // unique per span ⇒ every pair is async/concurrent, timing-independent
			Attrs:     attrs,
		})
	}
	// Assign (optionally reversed) start times after the structure is fixed, so the
	// permutation changes only the race outcome, never who is concurrent with whom.
	for i := 0; i < n; i++ {
		s, d := starts[i], durs[i]
		if permuteStarts {
			s, d = starts[n-1-i], durs[n-1-i]
		}
		spans[i+1].Start = ms(0, 1+s)
		spans[i+1].End = ms(0, 1+s+d)
	}
	return capture.CapturedFlow{Flow: "f", Service: "s", Spans: spans, Root: &spans[0], Complete: true}
}

var (
	fuzzDBStmts = []string{"INSERT INTO items (id) VALUES (1)", "SELECT * FROM users", "UPDATE accounts SET x=1", "DELETE FROM sessions"}
	fuzzTopics  = []string{"loan.approved", "order.created", "user.signup"}
	fuzzPeers   = []string{"payment-gw", "fraud-svc", "ledger"}
)

// byteReader hands out the fuzz input one byte at a time, returning 0 once
// exhausted so the generator is total over any input length.
type byteReader struct {
	data []byte
	pos  int
}

func (r *byteReader) b() byte {
	if r.pos >= len(r.data) {
		return 0
	}
	v := r.data[r.pos]
	r.pos++
	return v
}
