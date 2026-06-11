// Package billing is the fixture's fallible downstream call: the disburse
// scenario's fault site.
package billing

// Charge attempts the charge; it can fail after the publish has happened.
func Charge(id string) error { return nil }
