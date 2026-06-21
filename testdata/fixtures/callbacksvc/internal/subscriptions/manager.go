// Package subscriptions wires the inbound handler into the inbox via the
// manager-holds-handler idiom: the handler value threads through Config →
// constructor → struct field, so at the inbox.Register call site it is a field
// LOAD, not a literal. That is the value-flow chain root discovery cannot follow
// before the call graph exists.
package subscriptions

import (
	"example.com/callbacksvc/internal/inbox"
)

// Config carries the handler into the manager.
type Config struct {
	Handler inbox.Handler
}

// Manager holds the registered handler and the inbox it registers into.
type Manager struct {
	handler inbox.Handler
	ib      *inbox.Inbox
}

// New stores cfg.Handler on the manager — the field assignment the resolver bottoms
// out at when it tries to trace the registration backwards.
func New(cfg Config) *Manager {
	return &Manager{handler: cfg.Handler, ib: inbox.New()}
}

// Start registers m.handler. The argument m.handler is a struct-field load, so the
// resolver records a blind spot and the handler is orphaned absent a declaration.
func (m *Manager) Start() {
	inbox.Register(m.ib, m.handler)
}
