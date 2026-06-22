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

// --- Interface tx-runner: the dominant real shape, where the unit-of-work is an
// INTERFACE so the runner call inside the generic wrapper is INTERFACE-dispatched (no
// static callee). The reclaimer must resolve the interface method to its concrete
// implementation(s) to prove the wrapper closure — and thus the command's closure — is
// invoked; otherwise the write stays orphaned exactly as with the concrete runner. ---

// TxRunner is the unit-of-work as an INTERFACE.
type TxRunner interface {
	RunInTx(ctx context.Context, fn func(*Exec) error) error
}

// SQLRunner is the concrete implementation; like UnitOfWork.RunInTx it invokes fn directly.
type SQLRunner struct{ db *sql.DB }

func (r *SQLRunner) RunInTx(ctx context.Context, fn func(*Exec) error) error {
	e := &Exec{db: r.db}
	return fn(e)
}

// RunInTxIface is the generic wrapper over the INTERFACE runner: it wraps fn into a
// closure handed to u.RunInTx — an interface-dispatched call with no static callee.
func RunInTxIface[T any](ctx context.Context, u TxRunner, fn func(*Exec) (T, error)) (T, error) {
	var out T
	err := u.RunInTx(ctx, func(e *Exec) error {
		v, ferr := fn(e)
		out = v
		return ferr
	})
	return out, err
}

// IfaceCommand runs its write through the generic wrapper over the INTERFACE runner —
// the orphaned shape real event-bus commands take.
type IfaceCommand struct {
	u  TxRunner
	st *Store
}

func (c *IfaceCommand) Handle(ctx context.Context) error {
	_, err := RunInTxIface(ctx, c.u, func(e *Exec) (int, error) {
		return c.st.InsertSubscription(ctx, e)
	})
	return err
}

// --- Adversarial: an interface runner with TWO implementations, one of which does NOT
// invoke fn. The reclaimer must ABSTAIN — it cannot prove the closure is invoked, because
// the dynamic implementation might be the lazy one. This pins that the recovery requires
// EVERY implementation to invoke the parameter, not merely some (a sound-direction guard). ---

// MaybeRunner has two impls with divergent behavior.
type MaybeRunner interface {
	RunInTx(fn func(*Exec) error) error
}

// EagerRunner invokes fn; LazyRunner only STORES it (never invokes).
type EagerRunner struct{}

func (EagerRunner) RunInTx(fn func(*Exec) error) error { return fn(nil) }

type LazyRunner struct{ saved func(*Exec) error }

func (l *LazyRunner) RunInTx(fn func(*Exec) error) error { l.saved = fn; return nil }

func runMaybe(u MaybeRunner, fn func(*Exec) error) error { return u.RunInTx(fn) }

// MaybeCommand dispatches its closure through MaybeRunner — the reclaimer must NOT connect
// it, because LazyRunner (a possible dynamic type) never invokes the closure.
type MaybeCommand struct {
	u  MaybeRunner
	st *Store
}

func (c *MaybeCommand) Handle() error {
	return runMaybe(c.u, func(e *Exec) error {
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
	_ = (&IfaceCommand{u: &SQLRunner{db: db}, st: st}).Handle(ctx)
	// Box BOTH MaybeRunner impls so the call graph's runtime type set contains each.
	_ = (&MaybeCommand{u: EagerRunner{}, st: st}).Handle()
	_ = (&MaybeCommand{u: &LazyRunner{}, st: st}).Handle()
}
