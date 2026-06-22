// Command txrunnersvc reproduces the higher-order tx-runner seam that orphans a
// writing closure under a context-insensitive call graph. A command hands a closure
// to a shared runner that invokes it; because the call into the runner (especially the
// GENERIC wrapper RunInTxResult[T]) is not resolved to a traversable edge, the closure
// subtree — and the INSERTs inside it — is severed from the command, so a write-surface
// gate reads a writing command as 0 effects (a false-clean). The TxClosure reclaimer
// (internal/static/reclaim) recovers the sound enclosing-fn→closure edge.
//
// Three shapes, exercised by the reclaim tests:
//   - CreateSubscriptionCommand: the GENERIC wrapper (RunInTxResult[T]) — the measured case.
//   - DirectCommand: the closure passed to a runner that invokes its parameter DIRECTLY.
//   - StashCommand: the R2 NEGATIVE — a closure handed to a runner that only STORES it
//     and never invokes it, so no execution path runs it and it must NOT be connected.
//
// Nothing is executed under static analysis.
package main

import (
	"context"
	"database/sql"
)

// Exec is the in-transaction handle a unit-of-work closure receives.
type Exec struct{ db *sql.DB }

// UnitOfWork is the shared transaction runner. RunInTx invokes its fn parameter
// DIRECTLY: handing a closure to it provably invokes that closure.
type UnitOfWork struct{ db *sql.DB }

func (u *UnitOfWork) RunInTx(ctx context.Context, fn func(*Exec) error) error {
	e := &Exec{db: u.db}
	return fn(e)
}

// RunInTxResult is the GENERIC wrapper: it wraps fn in a closure (capturing fn) and
// hands THAT closure to RunInTx, which invokes it; the wrapper loads-and-calls fn. So
// fn is provably invoked, transitively, when RunInTxResult runs.
func RunInTxResult[T any](ctx context.Context, u *UnitOfWork, fn func(*Exec) (T, error)) (T, error) {
	var out T
	err := u.RunInTx(ctx, func(e *Exec) error {
		v, ferr := fn(e)
		out = v
		return ferr
	})
	return out, err
}

// Store is the persistence layer; InsertSubscription is a classified write.
type Store struct{ db *sql.DB }

func (s *Store) InsertSubscription(ctx context.Context, _ *Exec) (int, error) {
	const q = "INSERT INTO event_type_subscriptions (id) VALUES ($1)"
	_, err := s.db.ExecContext(ctx, q, "s1")
	return 0, err
}

// CreateSubscriptionCommand runs its write through the GENERIC wrapper — the closure
// subtree the reclaimer recovers.
type CreateSubscriptionCommand struct {
	u  *UnitOfWork
	st *Store
}

func (c *CreateSubscriptionCommand) Handle(ctx context.Context) error {
	_, err := RunInTxResult(ctx, c.u, func(e *Exec) (int, error) {
		return c.writeSubscription(ctx, e)
	})
	return err
}

func (c *CreateSubscriptionCommand) writeSubscription(ctx context.Context, e *Exec) (int, error) {
	return c.st.InsertSubscription(ctx, e)
}

// DirectCommand runs its write through RunInTx DIRECTLY (closure passed to a runner
// that calls its parameter).
type DirectCommand struct {
	u  *UnitOfWork
	st *Store
}

func (c *DirectCommand) Handle(ctx context.Context) error {
	return c.u.RunInTx(ctx, func(e *Exec) error {
		_, err := c.st.InsertSubscription(ctx, e)
		return err
	})
}

// Registry STORES handlers and never invokes them — the seam behind the R2 negative.
type Registry struct{ fns []func(*Exec) error }

func (r *Registry) Register(fn func(*Exec) error) { r.fns = append(r.fns, fn) }

// StashCommand hands a closure to Register, which only stores it. The closure is never
// invoked, so TxClosure must NOT connect StashCommand.Handle to it (R2).
type StashCommand struct {
	reg *Registry
	st  *Store
}

func (c *StashCommand) Handle(_ context.Context) {
	c.reg.Register(func(e *Exec) error {
		_, err := c.st.InsertSubscription(context.Background(), e)
		return err
	})
}

func main() {
	db, _ := sql.Open("postgres", "")
	u := &UnitOfWork{db: db}
	st := &Store{db: db}
	ctx := context.Background()
	_ = (&CreateSubscriptionCommand{u: u, st: st}).Handle(ctx)
	_ = (&DirectCommand{u: u, st: st}).Handle(ctx)
	(&StashCommand{reg: &Registry{}, st: st}).Handle(ctx)
}
