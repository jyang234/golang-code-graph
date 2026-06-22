// Package store is the persistence layer over database/sql (a built-in DB
// classification hint, declared in .flowmap.yaml). Each method's SQL shape drives
// how the schema-drift check bins it:
//
//   - constant SQL → the labeler reads "INSERT <table>", a RESOLVED write the check
//     diffs against the migration-defined schema;
//   - non-constant SQL (PurgeStale) → the verb is unreadable, the label falls back
//     to the driver method name, and the check counts it as an OPAQUE write (the
//     db-call frontier the "no drift" claim is scoped against).
package store

import (
	"context"
	"database/sql"
)

// Store persists provisioning state. A nil *sql.DB is fine for static analysis.
type Store struct {
	db *sql.DB
}

// New returns a Store backed by db.
func New(db *sql.DB) *Store { return &Store{db: db} }

// InsertEventType writes a table the migrations define — clean.
func (s *Store) InsertEventType(ctx context.Context) error {
	const q = "INSERT INTO event_types (name) VALUES ($1)"
	_, err := s.db.ExecContext(ctx, q)
	return err
}

// InsertOutbox writes the library-owned outbox table — no migration creates it, so
// it is clean only once declared library-owned (the completeness condition).
func (s *Store) InsertOutbox(ctx context.Context) error {
	const q = "INSERT INTO provisioning_outbox (payload) VALUES ($1)"
	_, err := s.db.ExecContext(ctx, q)
	return err
}

// InsertQueueMessage writes a table the migrations create then DROP — drift, the
// `relation does not exist` hazard.
func (s *Store) InsertQueueMessage(ctx context.Context) error {
	const q = "INSERT INTO queue_messages (body) VALUES ($1)"
	_, err := s.db.ExecContext(ctx, q)
	return err
}

// InsertAudit writes a table no migration defines — genuine drift.
func (s *Store) InsertAudit(ctx context.Context) error {
	const q = "INSERT INTO audit_log (event) VALUES ($1)"
	_, err := s.db.ExecContext(ctx, q)
	return err
}

// PurgeStale runs a statement against a runtime-chosen table — an OPAQUE write: the
// SQL is non-constant, so the labeler cannot read the table (db-call frontier).
func (s *Store) PurgeStale(ctx context.Context, table string) error {
	q := "DELETE FROM " + table + " WHERE stale = true"
	_, err := s.db.ExecContext(ctx, q)
	return err
}
