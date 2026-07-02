package graphio

// MermaidRootedAt renders the per-entrypoint flowchart: the subgraph forward-
// reachable from one handler, scoped at RENDER time over an UNSCOPED graph. This is
// the honest alternative to scoping the graph at BUILD time (graphio.Build with an
// entry): a build-scoped graph drops the Frontier section ("rides unscoped builds
// only"), so a build-scoped per-handler view shows blind spots but silently omits
// frontier markers. Rendering from the unscoped graph keeps BOTH disclosure
// channels in the per-handler view, so a reviewer reading one handler's diagram
// sees the same "where the analysis goes dark" markers the whole-graph view shows.
//
// It returns ok=false if root resolves to no handler and no node — fail closed
// rather than render an empty or whole-graph diagram under a misleading scope label.

import (
	"strings"

	"github.com/jyang234/golang-code-graph/internal/static/frontier"
)

// MermaidRootedAt resolves root (an entry-point route/topic name, or a function
// FQN) to a handler, then renders everything forward-reachable from it. g must be
// an UNSCOPED graph (graphio.Build with entry==""), so its Frontier and Entrypoints
// sections are populated.
func (g *Graph) MermaidRootedAt(root string, opts MermaidOptions) (string, bool) {
	sub, notes, rootFn, ok := g.rootedSubgraph(root)
	if !ok {
		return "", false
	}
	opts.pinRoot = rootFn
	return sub.mermaid(opts, notes), true
}

// rootedSubgraph resolves root and builds the forward-reachable sub-graph that
// MermaidRootedAt renders, returning that sub-graph, the disclosure notes for what
// render-time scoping pruned, and the resolved root FQN. Split out from MermaidRootedAt
// so a test can render the very same sub-graph with and without the --root pin
// (opts.pinRoot) and assert the pin is inert for a root tier-collapse would keep anyway —
// without re-deriving this scoping logic in the test (CLAUDE.md: one source of truth).
func (g *Graph) rootedSubgraph(root string) (*Graph, []string, string, bool) {
	rootFn, ok := g.resolveRoot(root)
	if !ok {
		return nil, nil, "", false
	}

	reach := g.forwardReach(rootFn)

	// Build a sub-graph of only the reachable structure, then reuse the whole
	// renderer. Edges are kept when their source is reachable; because forwardReach
	// is a forward closure, a first-party target of a kept edge is reachable too, and
	// a boundary target rides along with its reachable source.
	sub := &Graph{
		Entrypoint: root,
		Algo:       g.Algo,
	}
	for _, n := range g.Nodes {
		if reach[n.FQN] {
			sub.Nodes = append(sub.Nodes, n)
		}
	}
	for _, e := range g.Edges {
		if reach[e.From] {
			sub.Edges = append(sub.Edges, e)
		}
	}

	// Blind spots are attributed to a first-party site FQN; keep those whose site is
	// in reach. One whose site is not reachable from this handler — a package-level
	// site (reflect/unsafe, no owning function), or an FQN in another handler's
	// subtree — is pruned here and DISCLOSED, so the per-handler view never silently
	// omits a blind spot the whole-graph view shows.
	droppedBlind := 0
	for _, b := range g.BlindSpots {
		if reach[b.Site] {
			sub.BlindSpots = append(sub.BlindSpots, b)
		} else {
			droppedBlind++
		}
	}

	// Frontier markers are attributed to an owning function (Owner) or a site FQN.
	// Keep a marker when either falls in reach; one with no reachable owner/site (a
	// marker keyed on an effect label, or owned by a function outside this handler's
	// reach) is pruned and DISCLOSED in parallel with the blind spots above, so the
	// rooted view is symmetrically honest about both disclosure channels.
	droppedFrontier := 0
	var markers []frontier.Marker
	if g.Frontier != nil {
		for _, m := range g.Frontier.Markers {
			if reach[m.Owner] || reach[m.Site] {
				markers = append(markers, m)
			} else {
				droppedFrontier++
			}
		}
		if len(markers) > 0 {
			sub.Frontier = &FrontierSection{Markers: markers}
		}
	}

	// Annotations decorate a blind spot by (Site, Kind); the rooted view must carry
	// those whose blind spot SURVIVED the prune above, or the per-handler diagram drops
	// the 🗒 context the whole-graph view shows. It did: sub.Annotations was left nil, so
	// rooted (--root) views rendered ZERO annotation notes and ZERO 🗒 markers even
	// though the whole-graph render carried them — the most-read diagram was the one
	// missing the boundary context (§21.B). Filter to the surviving (Site, Kind) set, in
	// parallel with the blind-spot prune; an annotation whose spot was pruned rides that
	// spot's "shown only in the whole-graph view" disclosure, so it is not lost silently.
	keptSpot := make(map[[2]string]bool, len(sub.BlindSpots))
	for _, b := range sub.BlindSpots {
		keptSpot[[2]string{b.Site, string(b.Kind)}] = true
	}
	for _, a := range g.Annotations {
		if keptSpot[[2]string{a.Site, a.Kind}] {
			sub.Annotations = append(sub.Annotations, a)
		}
	}

	var notes []string
	if droppedBlind > 0 {
		notes = append(notes, plural(droppedBlind, "blind spot")+
			" outside this handler's reach shown only in the whole-graph view")
	}
	if droppedFrontier > 0 {
		notes = append(notes, plural(droppedFrontier, "frontier marker")+
			" outside this handler's reach shown only in the whole-graph view")
	}
	return sub, notes, rootFn, true
}

// resolveRoot maps a user-supplied root to a handler FQN. It tries, in order: an
// exact entry-point Name match, an exact node FQN, an exact entry-point Fn, then a
// segment-wise route match (so "POST /loan-application" resolves a route the router
// mounted under a prefix, the same loose match triage uses). The Name-based steps
// (exact Name and segment-wise route) FAIL CLOSED when more than one DISTINCT
// handler matches — an ambiguous root abstains rather than resolving at whichever
// entry sorted first (M-12), symmetric with build-time resolveEntryRoot. A step
// that matches exactly one handler returns it; determinism holds because the
// per-step match sets are keyed on the handler Fn and Entrypoints/Nodes are
// canonically sorted.
func (g *Graph) resolveRoot(root string) (string, bool) {
	// Exact Name match is authoritative, but two entrypoints can share a Name and
	// resolve to DIFFERENT handlers; fail closed on that ambiguity rather than
	// returning whichever sorted first (M-12, symmetric with build-time
	// resolveEntryRoot — an ambiguous root abstains, it never resolves arbitrarily).
	byName := map[string]bool{}
	for _, e := range g.Entrypoints {
		if e.Name == root {
			byName[e.Fn] = true
		}
	}
	if len(byName) == 1 {
		for fn := range byName {
			return fn, true
		}
	} else if len(byName) > 1 {
		return "", false
	}
	for _, n := range g.Nodes {
		if n.FQN == root {
			return n.FQN, true
		}
	}
	for _, e := range g.Entrypoints {
		if e.Fn == root {
			return e.Fn, true
		}
	}
	// Segment-wise route match for a router-prefixed leaf pattern. Collect ALL
	// matches and fail closed when more than one DISTINCT handler matches, rather
	// than silently rooting at whichever entry happened to sort first — a wrong-but-
	// plausible root must abstain, not resolve arbitrarily (CLAUDE.md: fail closed).
	matched := map[string]bool{}
	for _, e := range g.Entrypoints {
		if routeMatches(e.Name, root) {
			matched[e.Fn] = true
		}
	}
	if len(matched) == 1 {
		for fn := range matched {
			return fn, true
		}
	}
	return "", false
}

// routeMatches reports whether a query route names the same endpoint as an
// entry-point Name, comparing method (if present) and path SEGMENT-WISE so a
// leaf-pattern entry ("/{id}/status") matches a fuller query and vice versa, but a
// non-segment-aligned suffix ("/loans" vs "/v2/loans-archive") never collides. It is
// deliberately permissive — a per-handler VIEW, never a gate — but anchored on the
// method and whole path segments; ambiguity is resolved by the caller failing closed.
func routeMatches(name, query string) bool {
	nm, np := splitRoute(name)
	qm, qp := splitRoute(query)
	if nm != "" && qm != "" && !strings.EqualFold(nm, qm) {
		return false
	}
	if np == qp {
		return true
	}
	ns, qs := pathSegments(np), pathSegments(qp)
	if len(ns) == 0 || len(qs) == 0 {
		return false // an empty path is never a wildcard
	}
	return segmentSuffix(ns, qs) || segmentSuffix(qs, ns)
}

// pathSegments splits a route path on '/' and drops empty segments, so "/v2/loans"
// and "v2/loans" both yield [v2 loans] and a trailing slash adds no phantom segment.
func pathSegments(path string) []string {
	var segs []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	return segs
}

// segmentSuffix reports whether short is a whole-segment suffix of long (every
// segment of short equals the corresponding trailing segment of long). Empty short
// is not a suffix — an empty query must not match every route.
func segmentSuffix(long, short []string) bool {
	if len(short) == 0 || len(short) > len(long) {
		return false
	}
	off := len(long) - len(short)
	for i := range short {
		if long[off+i] != short[i] {
			return false
		}
	}
	return true
}

// splitRoute separates an optional leading HTTP method from the path of a route
// name ("POST /loan-application" -> "POST", "/loan-application"). A topic with no
// method ("payment.settled") returns an empty method and the whole string as path.
func splitRoute(name string) (method, path string) {
	name = strings.TrimSpace(name)
	if i := strings.IndexByte(name, ' '); i >= 0 {
		return name[:i], strings.TrimSpace(name[i+1:])
	}
	return "", name
}

// forwardReach returns the set of first-party FQNs reachable forward from rootFn
// over the call edges (boundary targets are leaves, not first-party nodes, so they
// are reached via their edge rather than added to this set). The traversal is a
// plain BFS over a deterministic adjacency built from the sorted Edges.
func (g *Graph) forwardReach(rootFn string) map[string]bool {
	adj := map[string][]string{}
	for _, e := range g.Edges {
		if isBoundary(e.To) {
			continue
		}
		adj[e.From] = append(adj[e.From], e.To)
	}
	reach := map[string]bool{rootFn: true}
	queue := []string{rootFn}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range adj[cur] {
			if !reach[next] {
				reach[next] = true
				queue = append(queue, next)
			}
		}
	}
	return reach
}
