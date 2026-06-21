// Package store is the persistence layer over database/sql (the built-in DB
// classification hint). Its writes are the effect cone the declared callback
// recovers, and its read is the cone the declared worker recovers — both invisible
// to the reachable graph until their entry points are rooted.
package store

import (
	"context"
	"database/sql"
)

// Store persists message state. A nil *sql.DB is fine for static analysis; the
// methods are never executed.
type Store struct {
	db *sql.DB
}

// New returns a Store backed by db.
func New(db *sql.DB) *Store { return &Store{db: db} }

// Insert records a message — a classified write (constant SQL). This is the
// callback's recovered effect.
func (s *Store) Insert(ctx context.Context, msg string) error {
	const q = "INSERT INTO message_events (body) VALUES ($1)"
	_, err := s.db.ExecContext(ctx, q, msg)
	return err
}

// Scan reads pending work — the worker's recovered effect (a constant read,
// provably non-mutating).
func (s *Store) Scan(ctx context.Context) error {
	const q = "SELECT id FROM message_events WHERE pending = true"
	_, err := s.db.QueryContext(ctx, q)
	return err
}
