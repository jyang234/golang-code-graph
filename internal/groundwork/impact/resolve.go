package impact

import (
	"fmt"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
)

// Resolution is a symptom resolved against the graph. Matches are the suspect
// functions; when more than one node answers an ambiguous input, all are
// returned (Ambiguous) — the resolver never guesses. Possible carries
// <dynamic>-boundary candidates: effects the graph could not name statically
// that MIGHT be the symptom (a dynamic publish might be the missing event),
// flagged as such rather than silently included or dropped.
type Resolution struct {
	Matches   []string `json:"matches"`
	Ambiguous bool     `json:"ambiguous,omitempty"`
	Possible  []string `json:"possible,omitempty"`
}

// ResolveFrame resolves a stack frame to graph nodes. The matching contract,
// most precise first: the graph's own FQN form; the runtime frame form
// ("pkg.(*T).Method"); a TOKEN-BOUNDED suffix — the suffix must start at a
// name boundary ('.', '(', '*', '/' or the whole string), so "--frame User"
// does not match GetUser/SelectUser and inflate the suspect set to most of
// the service.
func ResolveFrame(ix *graph.Index, frame string) Resolution {
	if ix.Has(frame) {
		return Resolution{Matches: []string{frame}}
	}
	if fqn := frameToFQN(frame); ix.Has(fqn) {
		return Resolution{Matches: []string{fqn}}
	}
	var matches []string
	for _, fqn := range ix.Nodes() {
		if suffixAtBoundary(fqn, frame) {
			matches = append(matches, fqn)
		}
	}
	return Resolution{Matches: matches, Ambiguous: len(matches) > 1}
}

// suffixAtBoundary reports whether frame is a suffix of fqn starting at a
// token boundary.
func suffixAtBoundary(fqn, frame string) bool {
	if frame == "" || !strings.HasSuffix(fqn, frame) {
		return false
	}
	if len(fqn) == len(frame) {
		return true
	}
	switch fqn[len(fqn)-len(frame)-1] {
	case '.', '(', '*', '/':
		return true
	}
	return false
}

// frameToFQN converts a runtime pointer-receiver frame ("pkg.(*T).Method") to
// the graph's FQN form ("(*pkg.T).Method").
func frameToFQN(s string) string {
	if i := strings.Index(s, ".(*"); i >= 0 {
		return "(*" + s[:i] + "." + s[i+3:]
	}
	return s
}

// ResolveRoute resolves an HTTP route symptom to its handler via the graph's
// entrypoints section. Route names are REGISTRATION-SITE literals, so matching
// is segment-aware rather than exact-or-nothing (the never-guess contract,
// fourth application): a param segment on either side ({id}, :id, <id>, *)
// matches any single segment; a root with no method (stdlib HandleFunc)
// matches any queried method; and the shorter path may match as a SUFFIX of
// the longer (mount tolerance: the alert says /api/v1/loans, the registration
// site saw /loans). Multiple matches return all candidates flagged ambiguous.
// Routers outside root discovery's coverage (gin variadic, gorilla chains,
// gRPC) are simply absent — a loud no-match, never a guess.
func ResolveRoute(ix *graph.Index, route string) Resolution {
	qMethod, qSegs := splitRoute(route)
	matches := map[string]bool{}
	for _, ep := range ix.Entrypoints() {
		if ep.Kind != "http" {
			continue
		}
		eMethod, eSegs := splitRoute(ep.Name)
		if qMethod != "" && eMethod != "" && !strings.EqualFold(qMethod, eMethod) {
			continue
		}
		if routeSegsMatch(eSegs, qSegs) {
			matches[ep.Fn] = true
		}
	}
	m := setutil.SortedKeys(matches)
	return Resolution{Matches: m, Ambiguous: len(m) > 1}
}

// splitRoute separates an optional leading method token from the path and
// splits the path into segments. "POST /loans/{id}" → ("POST", [loans {id}]);
// "/transfer" → ("", [transfer]).
func splitRoute(s string) (method string, segs []string) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ' '); i > 0 && !strings.Contains(s[:i], "/") {
		method, s = s[:i], strings.TrimSpace(s[i+1:])
	}
	for _, seg := range strings.Split(strings.Trim(s, "/"), "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	return method, segs
}

// routeSegsMatch aligns the shorter segment list against the TAIL of the
// longer (mount tolerance) and compares segment-wise, with param tokens on
// either side acting as single-segment wildcards. An empty list matches only
// an empty list (the bare "/" route).
func routeSegsMatch(a, b []string) bool {
	short, long := a, b
	if len(short) > len(long) {
		short, long = long, short
	}
	if len(short) == 0 {
		return len(long) == 0
	}
	long = long[len(long)-len(short):]
	for i := range short {
		if !routeSegEq(short[i], long[i]) {
			return false
		}
	}
	return true
}

func routeSegEq(x, y string) bool {
	return isParamSeg(x) || isParamSeg(y) || x == y
}

func isParamSeg(s string) bool {
	if s == "*" || s == "..." {
		return true
	}
	switch s[0] {
	case '{', ':', '<', '$':
		return true
	}
	return false
}

// ResolveTable resolves a DB table to the functions that touch it (any
// boundary:db edge whose label names the table).
func ResolveTable(ix *graph.Index, table string) Resolution {
	return resolveEffect(ix, "boundary:db ", table)
}

// ResolveEvent resolves a bus event to its publishers and consumers. The
// consumer side prefers the graph's entrypoints join (the actual handler
// function) over the CONSUME edge's source (the registration site, typically
// main/run wiring) — both are kept when both exist, since dropping evidence
// is guessing in the other direction.
func ResolveEvent(ix *graph.Index, event string) Resolution {
	res := resolveEffect(ix, "boundary:bus ", event)
	set := setutil.StringSet(res.Matches)
	for _, ep := range ix.Entrypoints() {
		if ep.Kind == "consumer" && ep.Name == event {
			set[ep.Fn] = true
		}
	}
	res.Matches = setutil.SortedKeys(set)
	res.Ambiguous = len(res.Matches) > 1
	return res
}

// ResolvePeer resolves an outbound peer to its callers (boundary:<peer> edges).
func ResolvePeer(ix *graph.Index, peer string) Resolution {
	return resolveEffect(ix, "boundary:"+peer+" ", "")
}

// resolveEffect matches boundary edges by label prefix and an optional token
// that must appear in the label's fields. Edges whose label is <dynamic> under
// the same prefix are possible matches, returned flagged, never guessed.
func resolveEffect(ix *graph.Index, prefix, token string) Resolution {
	matches, possible := map[string]bool{}, map[string]bool{}
	for _, fqn := range ix.Nodes() {
		for _, e := range ix.Effects(fqn) {
			if !strings.HasPrefix(e.To, prefix) {
				continue
			}
			switch {
			case token == "" || hasField(e.To, token):
				matches[e.From] = true
			case e.IsDynamic():
				possible[e.From] = true
			}
		}
	}
	m := setutil.SortedKeys(matches)
	return Resolution{Matches: m, Ambiguous: len(m) > 1, Possible: setutil.SortedKeys(possible)}
}

func hasField(label, token string) bool {
	for _, f := range strings.Fields(label) {
		if f == token {
			return true
		}
	}
	return false
}

// Render is the human-facing card — what a responder reads before opening
// anything else. The closing lines are the honest limits and the handoff.
func (c Card) Render() string {
	var b strings.Builder
	if c.Fault {
		b.WriteString("What-if: the suspects below are hypothesized to be failing.\n\n")
	}
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "%s (%d)\n", title, len(items))
		for _, it := range items {
			fmt.Fprintf(&b, "- %s\n", it)
		}
		b.WriteString("\n")
	}
	section("Suspects", c.Suspects)
	entryTitle := "Implicated entrypoints"
	if c.Fault {
		entryTitle = "Entrypoints degraded if the suspects fail"
	}
	if c.CoverOverApprox {
		// The reverse reach crossed a HighFanOut dispatch — the set is an upper
		// bound (every caller fanned onto every implementation), not a count.
		entryTitle += " ≤ (over-approx via dispatch)"
	}
	section(entryTitle, c.Entrypoints)
	section("⚡ Effects CERTAINLY committed before the fault", c.CertainlyCommitted)
	section("⚡ Effects possibly committed before the fault", c.PossiblyCommitted)
	section("Upstream callers", c.Callers)
	section("Reachable boundary effects", c.Effects)
	if len(c.BlindSpots) > 0 {
		fmt.Fprintf(&b, "🕳️  Blind spots on the traversed paths (%d) — claims above are unsound past these\n", len(c.BlindSpots))
		graph.WriteBlindSpots(&b, c.BlindSpots, c.Annotations, func(s graph.BlindSpot) string {
			row := "- " + s.Kind + " " + s.Site
			if s.Detail != "" {
				row += " — " + s.Detail
			}
			return row
		})
		b.WriteString("\n")
	}
	b.WriteString("This card is the map (what the suspects COULD touch), not the route taken.\n")
	b.WriteString("With an OTel trace of the failing request, `flowmap behavior ingest` locates\n")
	b.WriteString("the actual divergence inside this suspect set.\n")
	if c.Fault {
		// The fault card's epistemic scope, stated where over-reading happens —
		// next to the evidence. (a) The card bounds the CODE-shaped hypothesis
		// space only; (b) the effect-order coverage limit is voiced precisely as
		// coverage, not correctness, so listed (sound) facts are not discounted
		// — and it prints even when the sections are empty, which is exactly
		// when absence must not read as an all-clear.
		b.WriteString("Scope: causes outside the code (config, infra, data, deploys) are not on\n")
		b.WriteString("this map. Committed-effect facts cover same-function orderings only; an\n")
		b.WriteString("effect committed in a caller before the faulting call is not listed.\n")
	}
	return b.String()
}
