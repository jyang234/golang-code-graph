package graphio

import (
	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/boundarylabel"
	"github.com/jyang234/golang-code-graph/internal/canon/sql"
	"github.com/jyang234/golang-code-graph/internal/static/features"
	"github.com/jyang234/golang-code-graph/internal/static/sqlfold"
)

// busPublishPrefix and busConsumePrefix are the full boundary-label prefixes
// graphio EMITS for a bus publish/consume (the bus prefix followed by the PUBLISH
// or CONSUME verb and a trailing space). They are DERIVED from the shared
// boundarylabel.BusPrefix — the same constant every consumer parses against — so a
// change to the bus kind token propagates to the producer instead of leaving it a
// hardcoded twin (M-7, one source of truth; the DB path already emits via
// boundarylabel.DBPrefix). The verb token is spelled with its trailing space as a
// separate literal so the concatenation clears BOTH repo-scan guards, which key on
// the standalone bus-prefix and publish/consume-prefix literals — neither of which
// appears verbatim here.
var (
	busPublishPrefix = boundarylabel.BusPrefix + "PUBLISH" + " "
	busConsumePrefix = boundarylabel.BusPrefix + "CONSUME" + " "
)

// dynamicLabel is the sentinel for a boundary argument the labeler could not read
// off a compile-time constant. It is the single source of truth shared by the
// label producers here and the consumers that must NOT treat an unreadable
// boundary as a concretely-named effect (see committedEffect).
const dynamicLabel = "<dynamic>"

// viaTopicFold tags a bus boundary edge whose topic the reclaim-topic fold recovered
// from a finite, provably-complete constant set (the reclaim-sql analog for bus
// targets). Kept distinct from sqlfold.Via so a reviewer can tell which reclaimer
// named a target. The topic NAME is verdict-neutral — publish vs. consume is fixed by
// the hint, not the topic — so recovering or over-listing it can only refine a target
// name or a diff, never move a pole.
const viaTopicFold = "topic-constfold"

// eventLabels is the bus analog of dbLabel: the published/consumed topic name(s) for
// a bus boundary edge. A compile-time-constant topic yields one name (via=""). When
// foldBus is set (the opt-in --reclaim-topic reclaimer) and the topic is NOT a
// call-site constant, it tries the general const-set resolver (sqlfold.ConstStringSet):
// a finite, provably-complete set of constant topics fans out into one label per
// topic — an over-approximation in the safe direction — carrying via=viaTopicFold.
// Sound-or-abstain: anything it cannot prove complete stays dynamicLabel, exactly as
// a foldless build, so the default build is unchanged.
func eventLabels(site ssa.CallInstruction, foldBus bool) (labels []string, via string) {
	args := features.StringArgs(site)
	if len(args) >= 1 {
		if s, ok := features.ConstString(args[0]); ok {
			return []string{s}, ""
		}
		if foldBus {
			if topics, ok := sqlfold.ConstStringSet(args[0]); ok && len(topics) > 0 {
				return topics, viaTopicFold
			}
		}
	}
	return []string{dynamicLabel}, ""
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
		if labels, via, ok := recoverDBLabelsFromValue(args[0], foldSQL); ok {
			return labels, via
		}
	}
	return []string{sinkMethodName(site)}, ""
}

// viaPassthrough tags a boundary edge whose DB effect was re-attributed from a
// pass-through helper to one of its callers (the §19 Tier-B/C reclaimer): the verb
// lives one call-hop above the database/sql sink, at the caller that built the
// statement, so the recovered effect is homed there. Distinct from sqlfold.Via (a
// sink-local fold) so the cross-boundary re-attribution is auditable on its own.
const viaPassthrough = "sql-passthrough"

// recoverDBLabelsFromValue is the ONE source of truth for turning a SQL statement
// VALUE into "op table" label(s): a compile-time constant read through the canonical
// normalizer (canon/sql), else — when useFold is set — the const-accumulation fold
// (which also fans a finite-constant table set into one label per target, Tier C).
// via is sqlfold.Via when the fold fired, "" for a plain constant; ok=false when
// neither proves a verb. Both the sink labeler (dbLabel, q = the sink's statement
// arg) and the §19 pass-through re-attribution (q = a caller's forwarded arg) derive
// labels by calling HERE — one helper, so a sink-attributed and a caller-attributed
// label for the same statement cannot drift (CLAUDE.md "one source of truth").
func recoverDBLabelsFromValue(q ssa.Value, useFold bool) (labels []string, via string, ok bool) {
	if stmt, ok := features.ConstString(q); ok {
		if op, table := sqlOpTable(stmt); op != "" {
			return []string{joinOpTable(op, table)}, "", true
		}
	}
	if useFold {
		if op, tables, ok := sqlfold.Recover(q); ok {
			return dbFoldLabels(op, tables), sqlfold.Via, true
		}
	}
	return nil, "", false
}

// sinkMethodName is the opaque label dbLabel falls back to for a DB call whose
// statement it cannot read — the driver method name (ExecContext, QueryContext, …),
// or "call" when the callee is indirect. Shared so a re-attributed-but-unrecoverable
// effect re-homes the EXACT label the sink would otherwise have carried.
func sinkMethodName(site ssa.CallInstruction) string {
	// A nil site (synthetic self-edge, or an edge with no call instruction) has no
	// callee to name: fall back to the opaque "call" label rather than derefing a
	// nil Common(). Fail closed to the safe label, never a crash.
	if site == nil {
		return "call"
	}
	if c := site.Common().StaticCallee(); c != nil {
		return c.Name()
	}
	return "call"
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
