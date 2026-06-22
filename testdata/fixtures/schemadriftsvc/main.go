// Command schemadriftsvc is the schema-drift cross-check fixture. main is a root,
// so every store method it calls is reachable and its DB boundary edge appears in
// the emitted graph. The store deliberately writes five shapes the check must bin:
// a defined table, a library-owned outbox, a dropped table, an undefined table, and
// an opaque (non-constant SQL) write. See db/migrations and .flowmap.yaml.
package main

import (
	"context"
	"database/sql"

	"example.com/schemadriftsvc/store"
)

func main() {
	var db *sql.DB // nil is fine: static analysis never executes these.
	s := store.New(db)
	ctx := context.Background()

	_ = s.InsertEventType(ctx)    // event_types — defined by V1 (clean)
	_ = s.InsertOutbox(ctx)       // provisioning_outbox — library-owned (clean once declared)
	_ = s.InsertQueueMessage(ctx) // queue_messages — created V1, dropped V2 (drift)
	_ = s.InsertAudit(ctx)        // audit_log — defined by no migration (drift)
	_ = s.PurgeStale(ctx, "any")  // opaque: non-constant SQL (db-call frontier)
}
