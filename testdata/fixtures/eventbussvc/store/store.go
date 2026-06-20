// Package store is the persistence layer over database/sql (a built-in DB
// classification hint) — the domain effect the create handler commits.
package store

import (
	"context"
	"database/sql"
)

// Store persists event-type state. A nil *sql.DB is fine for static analysis;
// the methods are never executed.
type Store struct {
	db *sql.DB
}

// New returns a Store backed by db.
func New(db *sql.DB) *Store { return &Store{db: db} }

// DeleteOutbox removes expired outbox rows — a classified write (constant SQL).
func (s *Store) DeleteOutbox(ctx context.Context) error {
	const q = "DELETE FROM provisioning_outbox WHERE expired = true"
	_, err := s.db.ExecContext(ctx, q)
	return err
}

// Ping is a constant read — provably non-mutating.
func (s *Store) Ping(ctx context.Context) error {
	const q = "SELECT 1 FROM heartbeat"
	_, err := s.db.QueryContext(ctx, q)
	return err
}
