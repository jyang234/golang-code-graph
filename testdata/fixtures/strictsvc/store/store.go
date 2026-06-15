// Package store is the persistence layer over database/sql (a built-in DB
// classification hint). It deliberately mixes three DB-effect shapes so the
// frontier classifier has all of them to bin:
//
//   - DeleteOutbox: CONSTANT SQL → the labeler reads the verb → "db DELETE
//     provisioning_outbox" (a classified write — this is the one behind the
//     strict-server seam).
//   - ExecRaw: NON-CONSTANT SQL (built at runtime) → the verb is unreadable →
//     the label falls back to the method name "db ExecContext" (an opaque write,
//     the db-call frontier).
//   - Ping: CONSTANT read → "db SELECT" (provably non-mutating control).
package store

import (
	"context"
	"database/sql"
)

// Store persists provisioning state. A nil *sql.DB is fine for static analysis;
// the methods are never executed.
type Store struct {
	db *sql.DB
}

// New returns a Store backed by db.
func New(db *sql.DB) *Store { return &Store{db: db} }

// DeleteOutbox removes expired outbox rows — a CLASSIFIED write (constant SQL).
func (s *Store) DeleteOutbox(ctx context.Context) error {
	const q = "DELETE FROM provisioning_outbox WHERE expired = true"
	_, err := s.db.ExecContext(ctx, q)
	return err
}

// ExecRaw runs a statement against a runtime-chosen table — an OPAQUE write: the
// SQL is non-constant, so the labeler cannot read the verb (db-call frontier).
func (s *Store) ExecRaw(ctx context.Context, table string) error {
	q := "DELETE FROM " + table + " WHERE stale = true"
	_, err := s.db.ExecContext(ctx, q)
	return err
}

// Ping is a constant read — provably non-mutating.
func (s *Store) Ping(ctx context.Context) error {
	const q = "SELECT 1 FROM heartbeat"
	_, err := s.db.QueryContext(ctx, q)
	return err
}
