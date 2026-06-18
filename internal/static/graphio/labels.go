package graphio

import (
	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/canon/sql"
	"github.com/jyang234/golang-code-graph/internal/static/features"
	"github.com/jyang234/golang-code-graph/internal/static/sqlfold"
)

// dynamicLabel is the sentinel for a boundary argument the labeler could not read
// off a compile-time constant. It is the single source of truth shared by the
// label producers here and the consumers that must NOT treat an unreadable
// boundary as a concretely-named effect (see committedEffect).
const dynamicLabel = "<dynamic>"

// eventLabel is the published event name, or dynamicLabel if not constant.
func eventLabel(site ssa.CallInstruction) string {
	args := features.StringArgs(site)
	if len(args) >= 1 {
		if s, ok := features.ConstString(args[0]); ok {
			return s
		}
	}
	return dynamicLabel
}

// httpLabel is "peer method route" for a constant outbound call, else dynamicLabel.
func httpLabel(site ssa.CallInstruction) string {
	args := features.StringArgs(site)
	if len(args) >= 3 {
		p, ok1 := features.ConstString(args[0])
		m, ok2 := features.ConstString(args[1])
		r, ok3 := features.ConstString(args[2])
		if ok1 && ok2 && ok3 {
			return p + " " + m + " " + r
		}
	}
	return dynamicLabel
}

// dbLabel is the SQL operation and table ("SELECT applicants"), derived from the
// statement constant; it falls back to the DB method name. It shares the one
// canonical SQL normalizer (canon/sql) with the behavioral pipeline, so the
// static op/table cannot drift from the canonical op key.
//
// When foldSQL is set (the opt-in --reclaim-sql label reclaimer) and the
// statement is NOT a call-site constant, it tries the const-accumulation fold
// (internal/static/sqlfold) to recover the verb one accumulator-hop back; a
// recovered label carries via=sqlfold.Via so the verdict that reads it is
// auditable. The fold is sound-or-abstain, so a foldless build and a folded build
// differ only by labels the fold could PROVE — never by a guess.
func dbLabel(site ssa.CallInstruction, foldSQL bool) (labels []string, via string) {
	args := features.StringArgs(site)
	if len(args) >= 1 {
		if stmt, ok := features.ConstString(args[0]); ok {
			if op, table := sqlOpTable(stmt); op != "" {
				return []string{joinOpTable(op, table)}, ""
			}
		}
		if foldSQL {
			if op, tables, ok := sqlfold.Recover(args[0]); ok {
				return dbFoldLabels(op, tables), sqlfold.Via
			}
		}
	}
	if c := site.Common().StaticCallee(); c != nil {
		return []string{c.Name()}, ""
	}
	return []string{"call"}, ""
}

// dbFoldLabels renders a recovered op + table set into one label per table (a
// finite constant-set write fans out into one edge per possible target — an
// over-approximation in the safe direction), or a single bare-verb label when the
// table is dynamic. tables is already sorted by the fold, so the labels are
// deterministic.
func dbFoldLabels(op string, tables []string) []string {
	if len(tables) == 0 {
		return []string{op}
	}
	out := make([]string, 0, len(tables))
	for _, t := range tables {
		out = append(out, joinOpTable(op, t))
	}
	return out
}

// joinOpTable renders the DB label's "op table" form, dropping the table when it
// is unknown (a write whose target is dynamic, e.g. a fold-promoted DELETE).
func joinOpTable(op, table string) string {
	if table != "" {
		return op + " " + table
	}
	return op
}

// sqlOpTable extracts the leading SQL operation and primary table from a
// statement. It delegates to the canonical normalizer (canon/sql) — the single
// source of truth shared with the behavioral op key — so a view label and a
// canonical op key never disagree on the verb or table for the same statement.
func sqlOpTable(stmt string) (op, table string) {
	n := sql.Normalize(stmt)
	return n.Operation, n.Table
}
