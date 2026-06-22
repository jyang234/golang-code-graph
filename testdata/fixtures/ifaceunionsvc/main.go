// Command ifaceunionsvc is the INTERFACE-runner de-union fixture for --rebind. Two
// commands hand their OWN writing closure to a shared runner held as an INTERFACE
// (TxRunner). The interface call resolves to the concrete impl, whose single fn(exec) site
// fans onto EVERY closure flowing to it (the over-report union): CmdA appears to issue
// CmdB's write. The escape-guarded de-union ADDs each command's own enclosing→closure edge
// and REMOVEs the impl→closure union edges, so each command reaches only its own write.
//
// LeakCmd is the confinement adversary: its closure is passed both to the runner and to a
// helper, so it ESCAPES and its union must be kept. MaybeCmd is the soundness adversary:
// it dispatches through a SECOND interface (MaybeRunner) that has an eager AND a lazy impl,
// so the de-union must ABSTAIN (not every impl invokes → a stored closure could be invoked
// elsewhere → removing the union would be a false absence).
//
// Nothing is executed under static analysis.
package main

import (
	"context"
	"database/sql"
)

// Exec is the in-transaction handle.
type Exec struct{ db *sql.DB }

// TxRunner is the unit-of-work as an INTERFACE; SQLRunner is its single impl and invokes
// fn directly.
type TxRunner interface {
	RunInTx(fn func(*Exec) error) error
}

type SQLRunner struct{ db *sql.DB }

func (r *SQLRunner) RunInTx(fn func(*Exec) error) error {
	e := &Exec{db: r.db}
	return fn(e)
}

// Store holds the per-command writes (distinct tables so the over-report is observable).
type Store struct{ db *sql.DB }

func (s *Store) writeA(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO table_a (id) VALUES ($1)", "1")
	return err
}

func (s *Store) writeB(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO table_b (id) VALUES ($1)", "1")
	return err
}

func (s *Store) writeLeak(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO table_leak (id) VALUES ($1)", "1")
	return err
}

// CmdA / CmdB are CONFINED: each closure's only use is the interface RunInTx call.
type CmdA struct {
	u  TxRunner
	st *Store
}

func (c *CmdA) Handle(ctx context.Context) error {
	return c.u.RunInTx(func(e *Exec) error { return c.st.writeA(ctx) })
}

type CmdB struct {
	u  TxRunner
	st *Store
}

func (c *CmdB) Handle(ctx context.Context) error {
	return c.u.RunInTx(func(e *Exec) error { return c.st.writeB(ctx) })
}

// invoke is a helper that ALSO invokes the closure handed to it.
func invoke(fn func(*Exec) error) error { return fn(&Exec{}) }

// LeakCmd hands its closure to BOTH the runner and the helper — the closure escapes, so the
// confinement guard must keep its union edge.
type LeakCmd struct {
	u  TxRunner
	st *Store
}

func (c *LeakCmd) Handle(ctx context.Context) error {
	fn := func(e *Exec) error { return c.st.writeLeak(ctx) }
	_ = invoke(fn)
	return c.u.RunInTx(fn)
}

// MaybeRunner is a SEPARATE interface (distinct method name, so SQLRunner does not
// implement it and these impls do not pollute TxRunner) with an eager AND a lazy impl.
type MaybeRunner interface {
	RunMaybe(fn func(*Exec) error) error
}

type EagerRunner struct{}

func (EagerRunner) RunMaybe(fn func(*Exec) error) error { return fn(nil) }

type LazyRunner struct{ saved func(*Exec) error }

func (l *LazyRunner) RunMaybe(fn func(*Exec) error) error { l.saved = fn; return nil }

// MaybeCmd dispatches through MaybeRunner — the de-union must ABSTAIN, because LazyRunner (a
// possible dynamic type) never invokes the closure.
type MaybeCmd struct {
	u  MaybeRunner
	st *Store
}

func (c *MaybeCmd) Handle(ctx context.Context) error {
	return c.u.RunMaybe(func(e *Exec) error { return c.st.writeA(ctx) })
}

func main() {
	db, _ := sql.Open("postgres", "")
	st := &Store{db: db}
	r := &SQLRunner{db: db}
	ctx := context.Background()
	_ = (&CmdA{u: r, st: st}).Handle(ctx)
	_ = (&CmdB{u: r, st: st}).Handle(ctx)
	_ = (&LeakCmd{u: r, st: st}).Handle(ctx)
	// Box BOTH MaybeRunner impls so the runtime type set contains each.
	_ = (&MaybeCmd{u: EagerRunner{}, st: st}).Handle(ctx)
	_ = (&MaybeCmd{u: &LazyRunner{}, st: st}).Handle(ctx)
}
