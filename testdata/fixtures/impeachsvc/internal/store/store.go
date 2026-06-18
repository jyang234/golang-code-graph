// Package store is the fixture's persistence layer over database/sql (a built-in
// DB classification hint). Each method opens a client-kind OTel span carrying
// db.system and db.statement, which the behavioral pipeline canonicalizes to a
// "DB postgres <OP> <table>" op, while the static labeler reads the CONSTANT SQL
// to emit a "boundary:db <OP> <table>" edge. The constant statement is what keeps
// the effect statically NAMED (not opaque) — load-bearing for the impeachment
// fixture: the DELETE must be a named, severable effect, not a disclosed
// opaque-SQL frontier marker.
package store

import (
	"context"
	"database/sql"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func tracer() trace.Tracer { return otel.Tracer("impeachsvc") }

// dbSpan starts a client-kind span describing one SQL statement.
func dbSpan(ctx context.Context, stmt string) (context.Context, trace.Span) {
	ctx, span := tracer().Start(ctx, "db.query", trace.WithSpanKind(trace.SpanKindClient))
	span.SetAttributes(
		attribute.String("db.system", "postgres"),
		attribute.String("db.statement", stmt),
	)
	return ctx, span
}

// Loans persists loan state. A nil *sql.DB is fine for static analysis (methods
// are never executed); a real driver is wired in for the behavioral harness.
type Loans struct {
	db *sql.DB
}

// New returns a Loans backed by db.
func New(db *sql.DB) *Loans { return &Loans{db: db} }

// InsertLoan records a new loan (a DB mutate effect). It is reached from the
// DISCOVERED entrypoint (POST /loan), so its effect is CONFIRMED-LIVE when a flow
// exercises it — the fixture's sound baseline.
func (l *Loans) InsertLoan(ctx context.Context, id string, amount int) error {
	const q = "INSERT INTO loans (id, amount) VALUES ($1, $2)"
	ctx, span := dbSpan(ctx, q)
	defer span.End()
	_, err := l.db.ExecContext(ctx, q, id, amount)
	return err
}

// Purge deletes the ledger (a DB mutate effect — the scary one). It is reached
// ONLY through the admin sub-router, which is registered via a custom (unhinted)
// registrar, so no DISCOVERED entrypoint reaches this effect: statically it has
// no owning route, yet behavior demonstrably reaches it. The SQL is constant so
// the effect stays statically NAMED (boundary:db DELETE ledger), not opaque.
func (l *Loans) Purge(ctx context.Context, loanID string) error {
	const q = "DELETE FROM ledger WHERE loan_id = $1"
	ctx, span := dbSpan(ctx, q)
	defer span.End()
	_, err := l.db.ExecContext(ctx, q, loanID)
	return err
}

// PurgeAudit deletes the ledger's audit trail — a SECOND named DB mutate effect
// reached only through the same missed admin route. With it the missed route
// impeaches TWO effects from one capture (DELETE ledger + DELETE audit_log),
// exercising per-effect witness separation and the deterministic multi-candidate
// witness sort on a real trace — the ordering the single-effect corpus never drove
// over more than one finding (the two differ in their effect key, the sort's primary
// discriminator). Constant SQL keeps it statically NAMED (boundary:db DELETE
// audit_log), the impeachment precondition.
func (l *Loans) PurgeAudit(ctx context.Context, loanID string) error {
	const q = "DELETE FROM audit_log WHERE loan_id = $1"
	ctx, span := dbSpan(ctx, q)
	defer span.End()
	_, err := l.db.ExecContext(ctx, q, loanID)
	return err
}
