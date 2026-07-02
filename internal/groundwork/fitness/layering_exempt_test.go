package fitness

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// exempted must bind From/To at identifier boundaries: an exception for the edge
// "app.Get → store.Find" must NOT silently also exempt the unrelated
// "app.GetUserAvatar → store.FindByID", which a bare strings.HasPrefix would,
// suppressing a real layering violation.
func TestExemptedBoundary(t *testing.T) {
	allow := []policy.Exception{{From: "svc/app.Get", To: "svc/store.Find"}}

	if !exempted(allow, "svc/app.Get", "svc/store.Find") {
		t.Error("the exact blessed edge must be exempt")
	}
	if exempted(allow, "svc/app.GetUserAvatar", "svc/store.FindByID") {
		t.Error("an unrelated edge sharing a name prefix must NOT be exempt — a real violation would be suppressed")
	}
}

// TestExemptedOneSidedEntry pins H-6: a one-sided exception (only To set, an
// empty From wildcard) must exempt BOTH a free-function edge and a method edge
// identically. The old bare MatchPrefix(from, "") returned false for a free
// function ('s' is an ident byte, no room after an empty prefix) but true for a
// method-shaped From (the leading '(' is a non-ident byte), so the same
// exception half-worked, split by the receiver shape of the edge.
func TestExemptedOneSidedEntry(t *testing.T) {
	// Empty From = wildcard: any caller into store.Find is blessed.
	allow := []policy.Exception{{To: "svc/store.Find"}}

	const freeFn = "svc/app.readRow"           // free function
	const method = "(*svc/app.Server).readRow" // method with a receiver
	const target = "svc/store.Find"

	if !exempted(allow, freeFn, target) {
		t.Error("a one-sided (To-only) exception must exempt a free-function edge")
	}
	if !exempted(allow, method, target) {
		t.Error("a one-sided (To-only) exception must exempt a method edge")
	}
	// Symmetric: an empty To wildcard must bind both target shapes too.
	allowFrom := []policy.Exception{{From: "svc/app.Get"}}
	if !exempted(allowFrom, "svc/app.Get", "svc/store.Find") {
		t.Error("a one-sided (From-only) exception must exempt a free-function target")
	}
	if !exempted(allowFrom, "svc/app.Get", "(*svc/store.DB).Find") {
		t.Error("a one-sided (From-only) exception must exempt a method target")
	}
}
