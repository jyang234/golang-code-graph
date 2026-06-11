// Package app holds one function per obligation verdict — the fixture shapes
// the path-obligations plan's table commits to.
package app

import (
	"example.com/obligsvc/internal/audit"
	"example.com/obligsvc/internal/bus"
	"example.com/obligsvc/internal/store"
)

func debit(t *store.Tx) error  { return nil }
func credit(t *store.Tx) error { return nil }

// Transfer leaks: the debit-failure return has no release (VIOLATED).
func Transfer(s *store.Store) error {
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	if err := debit(tx); err != nil {
		return err
	}
	if err := credit(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// TransferDefer is covered on every exit by the deferred rollback (SATISFIED).
func TransferDefer(s *store.Store) error {
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := debit(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// TransferOwn returns the open transaction: ownership leaves the function
// (CANT-PROVE).
func TransferOwn(s *store.Store) (*store.Tx, error) {
	tx, err := s.BeginTx()
	if err != nil {
		return nil, err
	}
	return tx, nil
}

// Disburse audits before publishing on every path (SATISFIED).
func Disburse(approved bool) {
	audit.Write("loan.approved")
	if approved {
		bus.Publish("loan.approved")
	}
}

// DisburseRacy publishes on a path that skipped the audit (VIOLATED).
func DisburseRacy(approved bool) {
	if approved {
		audit.Write("loan.approved")
	}
	bus.Publish("loan.approved")
}

// handle recovers in a deferred NAMED function: control can rejoin invisibly,
// so the analysis must abstain (CANT-PROVE).
func handle() { _ = recover() }

// TransferRecoverNamed abstains: recover via a deferred named function.
func TransferRecoverNamed(s *store.Store) error {
	defer handle()
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	return tx.Commit()
}

// TransferClosure is the errcheck-clean cleanup idiom: the deferred closure
// releasing the captured tx is in-frame and credited (SATISFIED).
func TransferClosure(s *store.Store) error {
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	defer func() { tx.Rollback() }()
	return tx.Commit()
}

// TransferAnnotate has a named result captured by an annotating defer: the
// failure branch must still be recognized through the alloc/load web
// (SATISFIED).
func TransferAnnotate(s *store.Store) (err error) {
	defer func() { err = annotate(err) }()
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	defer func() { tx.Rollback() }()
	return tx.Commit()
}

func annotate(err error) error { return err }

// TransferConcrete acquires with a concrete error type (*store.TxError): it is
// the err, not the resource, and its failure branch prunes (SATISFIED).
func TransferConcrete(s *store.Store) error {
	tx, terr := s.BeginTxC()
	if terr != nil {
		return terr
	}
	defer func() { tx.Rollback() }()
	return tx.Commit()
}

// HoldSem is the single-result error acquire: the failure branch must prune
// even with no tuple Extract (SATISFIED).
func HoldSem(s *store.Store) error {
	if err := s.Acquire(); err != nil {
		return err
	}
	defer s.Release()
	return nil
}

// DeferredPublish publishes via defer with no audit anywhere: the deferred B
// still happens and still needs its A (VIOLATED, and the rule must not read
// as inert).
func DeferredPublish() { defer bus.Publish("loan.approved") }

// DeferredPublishAudited: the audit dominates the defer registration
// (SATISFIED).
func DeferredPublishAudited() {
	audit.Write("loan.approved")
	defer bus.Publish("loan.approved")
}
