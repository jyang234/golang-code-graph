// Package store is the fixture's persistence layer over database/sql (a built-in
// DB classification hint). Its operations are DB boundary edges: present in the
// non-gated call graph but deliberately excluded from the gated inter-service
// boundary contract, because the database is the service's own store.
//
// Each method opens a client-kind OTel span carrying db.system and db.statement,
// which the behavioral pipeline canonicalizes to a "DB postgres <OP> <table>" op.
// The instrumentation is invisible to the static extractor's gated artifacts: it
// adds only third-party (OTel) edges, never a new publish/HTTP/reflect hint.
package store

import (
	"context"
	"database/sql"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// tracer is fetched per span (never cached at package init): the OTel
// global binds a cached delegating tracer to the first provider installed,
// which would route a second in-process test's spans to the first test's
// recorder. Fetching per span always resolves the current provider.
func tracer() trace.Tracer { return otel.Tracer("loansvc") }

// dbSpan starts a client-kind span describing one SQL statement.
func dbSpan(ctx context.Context, stmt string) (context.Context, trace.Span) {
	ctx, span := tracer().Start(ctx, "db.query", trace.WithSpanKind(trace.SpanKindClient))
	span.SetAttributes(
		attribute.String("db.system", "postgres"),
		attribute.String("db.statement", stmt),
	)
	return ctx, span
}

// Applicant is a row read from the applicants table.
type Applicant struct {
	Name   string
	Income int
}

// Loan is a row read from the loans table.
type Loan struct {
	ID     string
	Status string
}

// Loans persists loan-origination state. A nil *sql.DB is acceptable for static
// analysis (the methods are never executed); a real driver is wired in for the
// behavioral harness.
type Loans struct {
	db *sql.DB
}

// New returns a Loans backed by db.
func New(db *sql.DB) *Loans { return &Loans{db: db} }

// SelectApplicant reads one applicant by id (a DB read effect).
func (l *Loans) SelectApplicant(ctx context.Context, id string, out *Applicant) error {
	const q = "SELECT name, income FROM applicants WHERE id = $1"
	ctx, span := dbSpan(ctx, q)
	defer span.End()
	row := l.db.QueryRowContext(ctx, q, id)
	return row.Scan(&out.Name, &out.Income)
}

// SelectLoan reads one loan by id (a DB read effect).
func (l *Loans) SelectLoan(ctx context.Context, id string, out *Loan) error {
	const q = "SELECT id, status FROM loans WHERE id = $1"
	ctx, span := dbSpan(ctx, q)
	defer span.End()
	row := l.db.QueryRowContext(ctx, q, id)
	return row.Scan(&out.ID, &out.Status)
}

// InsertLedger records a disbursement (a DB mutate effect).
func (l *Loans) InsertLedger(ctx context.Context, loanID string, amount int) error {
	const q = "INSERT INTO ledger (loan_id, amount) VALUES ($1, $2)"
	ctx, span := dbSpan(ctx, q)
	defer span.End()
	_, err := l.db.ExecContext(ctx, q, loanID, amount)
	return err
}

// InsertAudit appends an audit record (a DB mutate effect, written off the
// fire-and-forget goroutine).
func (l *Loans) InsertAudit(ctx context.Context, loanID string) error {
	const q = "INSERT INTO audit_log (loan_id) VALUES ($1)"
	ctx, span := dbSpan(ctx, q)
	defer span.End()
	_, err := l.db.ExecContext(ctx, q, loanID)
	return err
}

// MarkPaid flips a loan to settled (a DB mutate effect, driven by the consumer).
func (l *Loans) MarkPaid(ctx context.Context, loanID string) error {
	const q = "UPDATE loans SET status = 'paid' WHERE id = $1"
	ctx, span := dbSpan(ctx, q)
	defer span.End()
	_, err := l.db.ExecContext(ctx, q, loanID)
	return err
}
