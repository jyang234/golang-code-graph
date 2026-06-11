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

// ResolveFrame resolves a stack frame to graph nodes. It accepts the graph's
// own FQN form, the runtime frame form ("pkg.(*T).Method"), or a suffix
// ("handler.Server).GetUser", "GetUser").
func ResolveFrame(ix *graph.Index, frame string) Resolution {
	if ix.Has(frame) {
		return Resolution{Matches: []string{frame}}
	}
	if fqn := frameToFQN(frame); ix.Has(fqn) {
		return Resolution{Matches: []string{fqn}}
	}
	var matches []string
	for _, fqn := range ix.Nodes() {
		if strings.HasSuffix(fqn, frame) || strings.HasSuffix(fqn, "."+frame) {
			matches = append(matches, fqn)
		}
	}
	return Resolution{Matches: matches, Ambiguous: len(matches) > 1}
}

// frameToFQN converts a runtime pointer-receiver frame ("pkg.(*T).Method") to
// the graph's FQN form ("(*pkg.T).Method").
func frameToFQN(s string) string {
	if i := strings.Index(s, ".(*"); i >= 0 {
		return "(*" + s[:i] + "." + s[i+3:]
	}
	return s
}

// ResolveTable resolves a DB table to the functions that touch it (any
// boundary:db edge whose label names the table).
func ResolveTable(ix *graph.Index, table string) Resolution {
	return resolveEffect(ix, "boundary:db ", table)
}

// ResolveEvent resolves a bus event to its publishers and consumer registrars.
func ResolveEvent(ix *graph.Index, event string) Resolution {
	return resolveEffect(ix, "boundary:bus ", event)
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
	section("Implicated entrypoints", c.Entrypoints)
	section("Upstream callers", c.Callers)
	section("Reachable boundary effects", c.Effects)
	if len(c.BlindSpots) > 0 {
		fmt.Fprintf(&b, "🕳️  Blind spots on the traversed paths (%d) — claims above are unsound past these\n", len(c.BlindSpots))
		for _, s := range c.BlindSpots {
			fmt.Fprintf(&b, "- %s %s", s.Kind, s.Site)
			if s.Detail != "" {
				fmt.Fprintf(&b, " — %s", s.Detail)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("This card is the map (what the suspects COULD touch), not the route taken.\n")
	b.WriteString("With an OTel trace of the failing request, `flowmap behavior ingest` locates\n")
	b.WriteString("the actual divergence inside this suspect set.\n")
	return b.String()
}
