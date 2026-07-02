// Package boundarylabel is the single source of truth for the boundary-effect
// LABEL grammar that flowmap's graphio emits and every consumer parses: the
// "boundary:" namespace prefix and the per-kind "boundary:db " / "boundary:bus "
// prefixes. Roughly ten packages re-typed these literals with no shared constant
// and no guard (the producer graphio, the fitness proposers, impact, impeach,
// schemadrift, …); a drift in one — or a new consumer that spells it slightly
// differently — silently mis-parses every DB/bus effect it touches. Hoisting the
// grammar here (the effectkind/opkey precedent, for the label prefixes) plus a
// repo-scan guard test retires that drift class (M-7, CLAUDE.md: a rule applied in
// two places lives in one constant, guarded by a test).
//
// Only the KIND prefixes ("boundary:db ", "boundary:bus ") live here. The bare
// "boundary:" namespace has its own long-standing home (graph.IsBoundary and the
// mermaid/frontier boundary predicates) and is not re-homed by this package.
package boundarylabel

import "strings"

const (
	// Prefix namespaces every boundary-effect target: "boundary:db INSERT users".
	Prefix = "boundary:"

	// KindDB and KindBus are the leading kind token that follows Prefix in a DB or
	// bus effect label ("db INSERT users", "bus PUBLISH orders").
	KindDB  = "db"
	KindBus = "bus"

	// DBPrefix and BusPrefix are the full per-kind prefixes graphio emits and every
	// consumer strips or matches: "boundary:db ", "boundary:bus ".
	DBPrefix  = Prefix + KindDB + " "
	BusPrefix = Prefix + KindBus + " "
)

// HasKind reports whether a boundary-effect target is of the given kind token
// ("db"/"bus"), i.e. carries the "boundary:<kind> " prefix.
func HasKind(to, kind string) bool { return strings.HasPrefix(to, Prefix+kind+" ") }
