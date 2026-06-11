// Package store is the fixture's resource lifecycle: BeginTx acquires, Commit
// and Rollback release.
package store

// Tx is an open transaction.
type Tx struct{ closed bool }

// Store hands out transactions.
type Store struct{}

// BeginTx acquires a transaction (the obligation's acquire anchor).
func (s *Store) BeginTx() (*Tx, error) { return &Tx{}, nil }

// Commit releases the transaction successfully.
func (t *Tx) Commit() error { t.closed = true; return nil }

// Rollback releases the transaction on failure.
func (t *Tx) Rollback() { t.closed = true }

// TxError is a concrete error type: the obligations analysis must treat it as
// the error result (types.Implements), not as the resource.
type TxError struct{ msg string }

// Error implements error.
func (e *TxError) Error() string { return e.msg }

// BeginTxC acquires a transaction with a concrete error type.
func (s *Store) BeginTxC() (*Tx, *TxError) { return &Tx{}, nil }

// Acquire is a single-result error acquire (semaphore shape).
func (s *Store) Acquire() error { return nil }

// Release releases the semaphore.
func (s *Store) Release() {}
