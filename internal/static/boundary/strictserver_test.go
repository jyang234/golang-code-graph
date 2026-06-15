package boundary_test

import (
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/callgraph"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
)

func strictsvcDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", "strictsvc")
}

// TestStrictServerForwardStarvation is a CHARACTERIZATION test of the
// oapi-codegen strict-server topology — the production-common shape the field
// report (R3/R7/R8 §6) says no fixture in the matrix had. It pins what the static
// pipeline emits TODAY so the frontier measurement is deterministic and CI-guarded.
//
// The strict generator builds each handler as a per-operation `$1` closure inside
// the ServerInterfaceWrapper method, wraps it through middleware, and dispatches
// it via the http.Handler INTERFACE. The call-graph builder does not cross that
// interface hop into the closure, so:
//
//   - the wrapper method chi registers as an HTTP route is a graph ROOT whose
//     forward cone is EMPTY — the route, statically, appears to do nothing;
//   - the real handler chain (strictHandler → server.Server → store → db) hangs
//     off the `$1` closure, which is itself an orphan root (no caller);
//   - so EVERY boundary effect — including the classified `db DELETE` — is
//     reachable only from a `$1` closure, never from the HTTP entrypoint that
//     owns it. Entrypoint→effect attribution ("what does POST /x touch?") returns
//     nothing for every route.
//
// This is the forward-starvation behind R3/R7: a Category-B frontier (reclaimable
// static structure, NOT runtime dynamism), and the pipeline discloses ZERO blind
// spots for it. This test pins the DEFAULT (un-reclaimed) build: the strict-server
// reclaimer (internal/static/reclaim, opt-in via `flowmap graph --reclaim`) now
// crosses exactly this seam and closes it — TestApplyReclaimersClosesSeam asserts
// the reclaimed graph attributes every effect to its route. The default stays
// starved by design (reclaimers are opt-in, D2), which is what this test guards.
func TestStrictServerForwardStarvation(t *testing.T) {
	res, err := analyze.Analyze(strictsvcDir(), callgraph.Options{Algo: callgraph.AlgoVTA})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}

	nodes := map[string]bool{}
	for _, n := range g.Nodes {
		nodes[n.FQN] = true
	}
	out := map[string][]string{}
	callers := map[string]int{}
	for _, e := range g.Edges {
		out[e.From] = append(out[e.From], e.To)
		if nodes[e.To] {
			callers[e.To]++
		}
	}
	reach := func(seed string) map[string]bool {
		seen := map[string]bool{seed: true}
		st := []string{seed}
		for len(st) > 0 {
			c := st[len(st)-1]
			st = st[:len(st)-1]
			for _, to := range out[c] {
				if !seen[to] {
					seen[to] = true
					if nodes[to] {
						st = append(st, to)
					}
				}
			}
		}
		return seen
	}

	// The three wrapper methods are the HTTP entrypoints, and each is a starved
	// root: a graph root (no caller) whose forward cone is just itself.
	wrapperRe := regexp.MustCompile(`ServerInterfaceWrapper\)\.[A-Za-z]+$`)
	wrappers := 0
	for fqn := range nodes {
		if !wrapperRe.MatchString(fqn) {
			continue
		}
		wrappers++
		if callers[fqn] != 0 {
			t.Errorf("wrapper %s should be a root (chi registers it); got %d caller(s)", fqn, callers[fqn])
		}
		if r := reach(fqn); len(r) != 1 {
			t.Errorf("STARVATION RECLAIMED for %s: forward cone now has %d node(s) — the seam was crossed. Update this characterization test (that is the win).", fqn, len(r))
		}
	}
	if wrappers != 3 {
		t.Fatalf("expected 3 ServerInterfaceWrapper HTTP entrypoints, found %d", wrappers)
	}

	// Every boundary effect — including the classified write — is severed from its
	// HTTP entrypoint: reachable only from a `$1` closure, not from any wrapper.
	httpReach := map[string]bool{}
	for fqn := range nodes {
		if wrapperRe.MatchString(fqn) {
			for n := range reach(fqn) {
				httpReach[n] = true
			}
		}
	}
	effects := map[string]bool{}
	for _, e := range g.Edges {
		if strings.HasPrefix(e.To, "boundary:") {
			effects[e.To] = true
		}
	}
	// The fixture is built to emit all five frontier shapes; guard that so a
	// label-format change cannot quietly empty the measurement.
	for _, want := range []string{
		"boundary:db DELETE provisioning_outbox", // classified write
		"boundary:db ExecContext",                // opaque write (db-call frontier)
		"boundary:bus PUBLISH <dynamic>",         // truly-dynamic topic
		"boundary:bus PUBLISH eventtype.created", // resolved publish
		"boundary:db SELECT heartbeat",           // clean read
	} {
		if !effects[want] {
			t.Errorf("expected effect %q in the graph; got %v", want, keys(effects))
		}
	}
	for eff := range effects {
		if httpReach[eff] {
			t.Errorf("ATTRIBUTION RECLAIMED: %s is now reachable from an HTTP entrypoint — the seam was crossed. Update this characterization test (that is the win).", eff)
		}
	}

	// And the disclosure gap: the pipeline reports zero blind spots for a service
	// whose entire effect surface is severed from its routes. The starvation is a
	// silent structural gap, not a disclosed one.
	if len(g.BlindSpots) != 0 {
		t.Logf("note: %d blind spot(s) now disclosed (was 0); the structural starvation may now be surfaced", len(g.BlindSpots))
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
