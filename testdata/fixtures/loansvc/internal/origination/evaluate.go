// Package origination is the fixture's core decision logic. It concentrates the
// constructs the static pipeline must handle: an errgroup-concurrent pair (a DB
// read alongside an interface-dispatched credit score), a decline branch that no
// happy-path flow exercises, constant publishes that populate the boundary
// contract, and one non-constant publish that becomes a blind spot.
package origination

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"example.com/loansvc/internal/client"
	"example.com/loansvc/internal/eventbus"
	"example.com/loansvc/internal/scoring"
	"example.com/loansvc/internal/store"
)

// tracer is fetched per span (never cached at package init): the OTel
// global binds a cached delegating tracer to the first provider installed,
// which would route a second in-process test's spans to the first test's
// recorder. Fetching per span always resolves the current provider.
func tracer() trace.Tracer { return otel.Tracer("loansvc") }

// threshold is the minimum credit score for approval.
const threshold = 600

// Application is the inbound loan request.
type Application struct {
	ID          string
	ApplicantID string
	Amount      int
	Status      string
}

// Decision is the outcome of evaluating an Application.
type Decision struct {
	Approved bool
}

// Evaluator runs the origination decision against the store, the scorer, the
// payment gateway, and the bus.
type Evaluator struct {
	store   *store.Loans
	scorer  scoring.Scorer
	gateway *client.Gateway
	bus     *eventbus.Bus
}

// NewEvaluator wires an Evaluator.
func NewEvaluator(s *store.Loans, sc scoring.Scorer, gw *client.Gateway, bus *eventbus.Bus) *Evaluator {
	return &Evaluator{store: s, scorer: sc, gateway: gw, bus: bus}
}

// Evaluate decides an application. The applicant read and the credit score run
// concurrently under errgroup; on a low score it declines (a branch no happy-path
// flow reaches); otherwise it charges, publishes, and disburses.
func (e *Evaluator) Evaluate(ctx context.Context, app Application) (Decision, error) {
	var applicant store.Applicant
	var score scoring.Score

	// evaluateApplication wraps just the concurrent read+score so the behavioral
	// pipeline sees a tier-3 internal node over a concurrent pair: salience
	// filtering drops it and promotes the pair directly under the entry.
	evalCtx, evalSpan := tracer().Start(ctx, "evaluateApplication", trace.WithSpanKind(trace.SpanKindInternal))
	g, gctx := errgroup.WithContext(evalCtx)
	g.Go(func() error { return e.store.SelectApplicant(gctx, app.ApplicantID, &applicant) })
	g.Go(func() error { return e.scorer.Score(gctx, app.ApplicantID, &score) })
	err := g.Wait()
	evalSpan.End()
	if err != nil {
		return Decision{}, err
	}

	if score.Value < threshold {
		// Decline path: a boundary effect (loan.declined) on a branch that the
		// happy-path flow never drives — the coverage-delta signal.
		if err := e.bus.Publish(ctx, "loan.declined", []byte(app.ID)); err != nil {
			return Decision{}, err
		}
		return Decision{Approved: false}, nil
	}

	if err := e.gateway.Charge(ctx, app.ID); err != nil {
		return Decision{}, err
	}
	if err := e.bus.Publish(ctx, "loan.approved", []byte(app.ID)); err != nil {
		return Decision{}, err
	}
	if err := e.bus.Publish(ctx, "disbursement.initiated", []byte(app.ID)); err != nil {
		return Decision{}, err
	}
	if err := e.notify(ctx, app.Status); err != nil {
		return Decision{}, err
	}

	e.disburse(ctx, app)
	return Decision{Approved: true}, nil
}

// notify publishes a status event whose name is computed at runtime. The static
// extractor cannot resolve the event name, so this publish is recorded as a
// NonConstantBoundaryArg blind spot rather than a named published event.
func (e *Evaluator) notify(ctx context.Context, status string) error {
	event := "loan." + status
	return e.bus.Publish(ctx, event, nil)
}
