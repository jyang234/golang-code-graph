package fitness

import (
	"fmt"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/effectkind"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
	"github.com/jyang234/golang-code-graph/internal/sqlverb"
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
	routes := RouteWrites(p, ix)
	// Carried on the Result so review's route-delta section reuses this exact
	// computation instead of repeating the per-route BFS.
	r.RouteWrites = routes
	unclassRoutes := 0
	blindRoutes := 0
	unclassEffectRoutes := 0
	unclassified := map[string]bool{}
	unclassEffects := map[string]bool{}
	for _, src := range setutil.SortedKeys(routes) {
		writes := routes[src].Writes
		if len(writes) > max {
			r.add(Finding{
				Rule:     "io_budget",
				Severity: Violation,
				Summary:  fmt.Sprintf("%s reaches %d write(s) over a budget of %d: %s", ShortName(src), len(writes), max, strings.Join(writes, ", ")),
				From:     src,
			})
		}
		if len(routes[src].Unclassified) > 0 {
			unclassRoutes++
			for _, l := range routes[src].Unclassified {
				unclassified[l] = true
			}
		}
		if len(routes[src].UnclassifiedEffects) > 0 {
			unclassEffectRoutes++
			for _, l := range routes[src].UnclassifiedEffects {
				unclassEffects[l] = true
			}
		}
		if routes[src].Blind {
			blindRoutes++
		}
	}
	// Two disclosures that keep a green budget from lying — both the io_budget
	// analogue of the must_not_reach blind-frontier caution, kept distinct because
	// the epistemic reasons differ.
	//
	// (1) A DB effect built from non-constant SQL is labeled "db call" (or a bare
	// method name), not "db INSERT/UPDATE/DELETE" — IsWrite cannot read it as a
	// write, so it counts as zero against the budget. A route whose mutations all
	// flow through such a wrapper reaches an unbounded write surface the cap
	// silently passes. We do not GUESS those are writes (that would manufacture
	// false violations); we surface a caution so "within budget" stops reading as
	// "writes are bounded" where the labeler went blind on the verb.
	if unclassRoutes > 0 {
		r.add(Finding{
			Rule:     "io_budget",
			Severity: Caution,
			Summary: fmt.Sprintf("write budget unenforceable on %d route(s): %d DB effect label(s) are unclassified (%s) — built from non-constant SQL the labeler cannot read as a write, so a within-budget pass here does not prove the write surface is bounded",
				unclassRoutes, len(unclassified), strings.Join(setutil.SortedKeys(unclassified), ", ")),
		})
	}
	// (1b) The non-DB analog of (1): a typed outbound effect (object storage) whose
	// operation is a method name the budget cannot read as a write. We do not guess
	// it mutates (a method-name heuristic would manufacture false violations); we
	// disclose that "within budget" does not prove the write surface is bounded where
	// such an effect is reached. Kept distinct from (1) because the kind differs.
	if unclassEffectRoutes > 0 {
		r.add(Finding{
			Rule:     "io_budget",
			Severity: Caution,
			Summary: fmt.Sprintf("write budget unenforceable on %d route(s): %d external effect label(s) (%s) whose operation the budget cannot read as a write — a within-budget pass here does not prove the write surface is bounded",
				unclassEffectRoutes, len(unclassEffects), strings.Join(setutil.SortedKeys(unclassEffects), ", ")),
		})
	}
	// (2) A route whose forward cone touches a blind frontier — a dynamic-dispatch
	// seam (HighFanOut), a reflect/unsafe site, or a <dynamic> boundary effect —
	// has a write count that is a LOWER BOUND, not a proof: edges past the
	// frontier are hidden, so writes reachable beyond it are uncounted. This is
	// the half that fires on the real oapi-codegen topology, where per-route
	// forward reach is starved at the strictHandler dispatch and a route can read
	// "0 writes" while its true cone is unknown.
	if blindRoutes > 0 {
		r.add(Finding{
			Rule:     "io_budget",
			Severity: Caution,
			Summary: fmt.Sprintf("write budget is a lower bound on %d route(s): the forward frontier is blind (dynamic dispatch, reflect/unsafe, or a <dynamic> effect) — writes reachable past it are uncounted, so a within-budget pass there is not a proof the write surface is bounded",
				blindRoutes),
		})
	}
}

// RouteIO is one route's external write surface: the sorted distinct write
// targets (sans "boundary:") reachable from it, and whether the route's cone
// touches a blind spot — in which case Writes is a lower bound, not a count.
//
// Unclassified is the sorted distinct DB effect labels in the cone whose SQL
// verb the labeler could not read (a "db call" / method-named label). These are
// NOT counted as writes — the budget cannot prove they mutate — but their
// presence means the write count is unenforceable here, which the budget check
// discloses rather than passing silently. The review surface reuses it to
// ratchet the unclassified-DB fraction base→branch.
type RouteIO struct {
	Writes       []string
	Blind        bool
	Unclassified []string
	// UnclassifiedEffects are typed outbound effects (object storage) whose write-ness
	// the budget cannot read — the non-DB analog of Unclassified. Their presence makes
	// the write count a non-proof, disclosed by checkIOBudget.
	UnclassifiedEffects []string
}

// RouteWrites computes the write surface of every route (non-root entrypoint),
// with checkIOBudget's exact semantics — one computation, shared with the
// review artifact's per-route delta section so the two surfaces can never
// disagree about what a route writes.
func RouteWrites(p *policy.Policy, ix *graph.Index) map[string]RouteIO {
	roots := p.RootPackages()
	out := map[string]RouteIO{}
	for _, src := range ix.Sources() {
		if isRootPkg(roots, PkgOf(src)) {
			continue // the composition root (main) is an entrypoint but not a route
		}
		cone := append([]string{src}, ix.Reachable(src)...)
		effects := ix.Effects(cone...)
		writes := map[string]bool{}
		unclassified := map[string]bool{}
		unclassEffects := map[string]bool{}
		for _, e := range effects {
			if label, ok := WriteLabel(e); ok {
				writes[label] = true
			}
			if label, ok := UnclassifiedDBLabel(e); ok {
				unclassified[label] = true
			}
			if label, ok := UnclassifiedEffectLabel(e); ok {
				unclassEffects[label] = true
			}
		}
		_, blind := frontierBlindSiteWith(ix, cone, effects)
		out[src] = RouteIO{
			Writes:              setutil.SortedKeys(writes),
			Blind:               blind,
			Unclassified:        setutil.SortedKeys(unclassified),
			UnclassifiedEffects: setutil.SortedKeys(unclassEffects),
		}
	}
	return out
}

// UnclassifiedDBLabel returns the label (sans "boundary:") of a DB effect whose
// SQL verb the labeler could not read AND which might therefore be an unproven
// write, plus whether e is one. The verb is "<system> <op> <target>".
//
// Two kinds of label are NOT unclassified-write frontiers and are excluded:
//
//   - A read/write SQL verb the labeler read (INSERT/UPDATE/DELETE/UPSERT/MERGE/
//     REPLACE/SELECT): IsWrite classifies it directly.
//   - A provably-non-mutating driver/transaction control method (Ping*, Begin*,
//     Commit, Rollback) or a connection-pool/session setter (Set*): these reach
//     the DB but cannot mutate a row, so a route reaching only these is not a
//     write surface the budget "silently passes" — flagging it is a false caution
//     that inflates the count and breeds caution-fatigue (R2).
//
// Everything else — "db call", a bare method name like ExecContext, and the
// read/exec methods Query*/QueryRow* (Postgres `INSERT … RETURNING` legitimately
// rides QueryContext, so a Query* MIGHT mutate) — is built from non-constant SQL
// the labeler cannot read as a write. Such an effect MIGHT mutate, but IsWrite
// cannot tell, so it is neither charged to the budget nor trusted as a read; it
// is the frontier the budget caution discloses.
func UnclassifiedDBLabel(e graph.Edge) (string, bool) {
	if !e.IsBoundary() {
		return "", false
	}
	f := strings.Fields(strings.TrimPrefix(e.To, "boundary:"))
	if len(f) < 2 || f[0] != "db" {
		return "", false
	}
	if op := strings.ToUpper(f[1]); sqlverb.Mutating(op) || op == "SELECT" {
		return "", false // a verb the labeler read and IsWrite can classify
	}
	if nonMutatingDBControl(f[1]) {
		return "", false // driver/transaction control or pool config — cannot mutate a row
	}
	return strings.TrimPrefix(e.To, "boundary:"), true
}

// nonMutatingDBControl reports whether a DB effect op is a driver/transaction
// control method or a connection-pool/session setter — a sql.DB/sql.Tx call that
// reaches the database but provably cannot write a row, so it must not count as
// an unclassified-write frontier. The op is the method name graphio fell back to
// when the statement was not a compile-time constant.
//
// The set mirrors the DB boundaries graphio actually emits: the labeler treats
// every database/sql method EXCEPT the result-cursor surface (Scan/Next/Close/…)
// as a boundary, "leaving the actual Query*/Exec* round-trips (and Ping/Begin/
// Prepare) as the only DB boundaries" (static/features/hints.go). Of those, only
// Query*/Exec* can execute a statement — Ping*, Begin*/Commit/Rollback, Conn,
// Stats, and Prepare*/Stmt-creation send no row-mutating SQL, and Set* is
// pool/session config. A constant transaction-control statement ("COMMIT", "SET
// search_path") lands here too. Matched case-insensitively to cover both the
// method-name and constant-SQL labels.
func nonMutatingDBControl(op string) bool {
	up := strings.ToUpper(op)
	switch up {
	case "PING", "PINGCONTEXT",
		"BEGIN", "BEGINTX",
		"COMMIT", "ROLLBACK",
		"CONN", "STATS",
		"PREPARE", "PREPARECONTEXT":
		return true
	}
	// The sql.DB pool/session setters are exactly SetMaxOpenConns,
	// SetMaxIdleConns, SetConnMaxLifetime, SetConnMaxIdleTime, plus the SQL
	// control statement "SET search_path". Match those specifically — a bare
	// "SET" prefix would also swallow a mutating method like "Settle".
	return up == "SET" || strings.HasPrefix(up, "SET ") ||
		strings.HasPrefix(up, "SETMAX") || strings.HasPrefix(up, "SETCONN")
}

// methodNamedEffect reports whether the boundary-label fields name a method-named
// outbound effect (blob/cache/rpc, per the shared effectkind set) — a kind token
// followed by exactly the callee method name, i.e. EXACTLY two fields. The two-field
// shape is load-bearing: an outbound HTTP edge is "<peer> <METHOD> <route>" (three
// fields), so requiring len==2 keeps an HTTP peer that happens to be NAMED "blob"/
// "cache"/"rpc" from colliding with a kind token and being mis-dropped from the
// write surface. The kind set itself lives in internal/effectkind (one source of
// truth with the static labeler that produces these labels).
func methodNamedEffect(f []string) bool {
	return len(f) == 2 && effectkind.IsMethodNamed(f[0])
}

// UnclassifiedEffectLabel returns the label (sans "boundary:") of a method-named
// outbound effect (blob/cache/rpc) whose write-ness the labeler cannot read, plus
// whether e is one. Kept SEPARATE from UnclassifiedDBLabel — that one is the
// non-constant-SQL DB frontier, reused by the DB-specific proposals/ratchet — so the
// two unenforceable-write frontiers stay independently auditable.
func UnclassifiedEffectLabel(e graph.Edge) (string, bool) {
	if !e.IsBoundary() {
		return "", false
	}
	f := strings.Fields(strings.TrimPrefix(e.To, "boundary:"))
	if !methodNamedEffect(f) {
		return "", false
	}
	return strings.TrimPrefix(e.To, "boundary:"), true
}

// WriteLabel returns the effect label (sans "boundary:") of an external write,
// and whether e is one — the single extraction point for the write surface.
// The budget, the effect-ratchet audit, and the review's new-target diff all
// label writes through here, so they cannot disagree about the format.
func WriteLabel(e graph.Edge) (string, bool) {
	if !IsWrite(e) {
		return "", false
	}
	return strings.TrimPrefix(e.To, "boundary:"), true
}

// IsWrite reports whether a boundary effect mutates external state. The effect
// label is "<system> <op> <target>": db with a mutating SQL verb, bus PUBLISH, or
// an outbound HTTP call with a mutating method. A method-named outbound effect
// (blob/cache/rpc) is NOT counted as a write — its write-ness is unreadable and
// disclosed as unenforceable instead (see methodNamedEffect / UnclassifiedEffectLabel).
// It is shared with the review
// surface, which classifies the same effects in an MR's I/O-effect section.
func IsWrite(e graph.Edge) bool {
	if !e.IsBoundary() {
		return false
	}
	f := strings.Fields(strings.TrimPrefix(e.To, "boundary:"))
	if len(f) < 2 {
		return false
	}
	if methodNamedEffect(f) {
		// A method-named outbound effect (blob/cache/rpc): the op is a method name the
		// budget does NOT read as a verb (a method-name heuristic could be silently
		// wrong, e.g. a method literally named "Delete"). Not counted as a write;
		// disclosed as unenforceable via UnclassifiedEffectLabel instead.
		return false
	}
	op := strings.ToUpper(f[1])
	switch f[0] {
	case "db":
		return sqlverb.Mutating(op) // SELECT and other reads are not writes
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
