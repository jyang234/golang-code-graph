package fitness

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// checkIOBudget caps the external *write* effects reachable from a single route
// — the side-effect-blowout guard. Each structural entrypoint (Sources) is judged
// independently, EXCEPT the composition root (main), which is an entrypoint but
// not a route and whose startup writes (migrations, seeding) must not be charged
// against a per-route budget. Reads (DB SELECT, outbound GET, bus consume) do not
// count, only mutations (DB INSERT/UPDATE/DELETE, bus PUBLISH, outbound
// POST/PUT/PATCH/DELETE). "Route" is approximated by "non-root entrypoint"; the
// boundary contract refines it to named HTTP routes and bus consumers.
func checkIOBudget(p *policy.Policy, ix *graph.Index, r *Result) {
	if p.IOBudget == nil {
		return
	}
	max := p.IOBudget.MaxWritesPerRoute
	roots := p.RootPackages()
	for _, src := range ix.Sources() {
		if isRootPkg(roots, PkgOf(src)) {
			continue // the composition root (main) is an entrypoint but not a route
		}
		cone := append([]string{src}, ix.Reachable(src)...)
		writes := map[string]bool{}
		for _, e := range ix.Effects(cone...) {
			if IsWrite(e) {
				writes[strings.TrimPrefix(e.To, "boundary:")] = true
			}
		}
		if len(writes) > max {
			r.add(Finding{
				Rule:     "io_budget",
				Severity: Violation,
				Summary:  fmt.Sprintf("%s reaches %d write(s) over a budget of %d: %s", ShortName(src), len(writes), max, strings.Join(sortedKeys(writes), ", ")),
				From:     src,
			})
		}
	}
}

// IsWrite reports whether a boundary effect mutates external state. The effect
// label is "<system> <op> <target>": db with a mutating SQL verb, bus PUBLISH, or
// an outbound HTTP call with a mutating method. It is shared with the review
// surface, which classifies the same effects in an MR's I/O-effect section.
func IsWrite(e graph.Edge) bool {
	if !e.IsBoundary() {
		return false
	}
	f := strings.Fields(strings.TrimPrefix(e.To, "boundary:"))
	if len(f) < 2 {
		return false
	}
	op := strings.ToUpper(f[1])
	switch f[0] {
	case "db":
		switch op {
		case "INSERT", "UPDATE", "DELETE", "UPSERT", "MERGE", "REPLACE":
			return true
		default: // SELECT and other reads
			return false
		}
	case "bus":
		return op == "PUBLISH"
	default: // outbound HTTP: "<peer> <METHOD> <route>"
		switch op {
		case "POST", "PUT", "PATCH", "DELETE":
			return true
		default: // GET, HEAD, OPTIONS
			return false
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
