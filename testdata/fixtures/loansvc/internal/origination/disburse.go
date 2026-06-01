package origination

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// disburse writes the ledger entry and then audits asynchronously. The audit
// runs in a fire-and-forget goroutine — a raw `go` statement, which is the
// *ssa.Go the static pipeline reads as a concurrency signal and which the
// behavioral harness must drain before a snapshot is complete. The disburse span
// is another tier-3 internal wrapper: salience filtering drops it and promotes
// the ledger write and the (sequential) audit write under the entry.
func (e *Evaluator) disburse(ctx context.Context, app Application) {
	ctx, span := tracer().Start(ctx, "disburse", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()
	if err := e.store.InsertLedger(ctx, app.ID, app.Amount); err != nil {
		return
	}
	go e.auditLog(context.WithoutCancel(ctx), app.ID)
}

// auditLog records an audit row. It is reachable only through the goroutine
// spawned in disburse.
func (e *Evaluator) auditLog(ctx context.Context, id string) {
	_ = e.store.InsertAudit(ctx, id)
}
