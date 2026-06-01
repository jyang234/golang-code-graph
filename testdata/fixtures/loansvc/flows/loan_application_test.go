// Package flows_test is the fixture's behavioral gate: it drives the real loansvc
// object graph through flowmap's PUBLIC harness and flow packages, exactly as an
// adopting service repository would. It proves the public surface compiles and
// gates across the module boundary (engine module imported from the fixture
// module via go.work), with the canonical golden committed under testdata/flows.
//
// Outbound dependencies are hermetic doubles: a fake database/sql driver and a
// fake HTTP transport, each adding a few milliseconds of latency so the
// concurrent applicant-read ∥ credit-score pair reliably overlaps and the
// captured concurrency is deterministic.
package flows_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/jyang234/golang-code-graph/flow"
	"github.com/jyang234/golang-code-graph/harness"

	"example.com/loansvc/internal/client"
	"example.com/loansvc/internal/consumer"
	"example.com/loansvc/internal/eventbus"
	"example.com/loansvc/internal/handler"
	"example.com/loansvc/internal/origination"
	"example.com/loansvc/internal/scoring"
	"example.com/loansvc/internal/store"
)

func init() { sql.Register("fakepg", fakeDriver{}) }

// wire builds the loansvc service over hermetic doubles and returns its router
// and bus, mirroring the production wiring in main.run.
func wire() (http.Handler, *eventbus.Bus, *consumer.Payments) {
	db, _ := sql.Open("fakepg", "")
	loans := store.New(db)

	hc := &http.Client{Transport: fakeTransport{}}
	c := client.NewWithClient(hc)
	bureau := client.NewBureau(c)
	gateway := client.NewGateway(c)
	scorer := scoring.Select(false, bureau)

	bus := eventbus.New()
	eval := origination.NewEvaluator(loans, scorer, gateway, bus)
	app := handler.New(eval, loans)
	payments := consumer.New(loans)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /loan-application", app.Create)
	mux.HandleFunc("GET /loan-application/{id}/status", app.Status)
	bus.Subscribe("payment.settled", payments.OnSettled)
	return mux, bus, payments
}

// TestLoanApplicationFlow gates the happy path through the real router. The
// tier-3 evaluateApplication and disburse wrappers are contracted away, leaving
// the concurrent read ∥ credit-score pair under the entry and the sequential
// charge → publishes → ledger → audit. Run with -update to rebase the golden.
func TestLoanApplicationFlow(t *testing.T) {
	mux, _, _ := wire()
	app := harness.NewInProcess(t, mux, harness.WithService("loansvc"))

	body := []byte(`{"ID":"L1","ApplicantID":"A1","Amount":5000,"Status":"review"}`)
	flow.New("POST /loan-application").
		TriggerBody("POST", "/loan-application", body).
		ExpectExactlyOnce("HTTP GET credit-bureau /score/{id}").
		ExpectExactlyOnce("HTTP POST payment-gw /charge/{id}").
		ExpectExactlyOnce("PUBLISH loan.approved").
		ExpectExactlyOnce("PUBLISH disbursement.initiated").
		ExpectExactlyOnce("DB postgres INSERT ledger").
		Expect("DB postgres INSERT audit_log").
		Quiescence(15*time.Millisecond, 3*time.Second).
		Run(t, app)
}

// TestPaymentSettledFlow gates the consumer-rooted flow: the payment.settled
// consumer span is the entry and the loan is marked paid.
func TestPaymentSettledFlow(t *testing.T) {
	_, _, payments := wire()
	app := harness.NewInProcess(t, http.NewServeMux(), harness.WithService("loansvc"))

	deliver := func(ctx context.Context) {
		ctx, span := otel.Tracer("loansvc").Start(ctx, "consume",
			oteltrace.WithSpanKind(oteltrace.SpanKindConsumer))
		span.SetAttributes(attribute.String("messaging.destination.name", "payment.settled"))
		defer span.End()
		_ = payments.OnSettled(ctx, []byte("L1"))
	}

	flow.New("consume payment.settled").
		TriggerEvent("payment.settled", deliver).
		ExpectExactlyOnce("DB postgres UPDATE loans").
		Quiescence(15*time.Millisecond, 3*time.Second).
		Run(t, app)
}

// --- hermetic doubles ---------------------------------------------------------

// fakeTransport answers every outbound peer call with 200 OK after a short
// latency, so the credit-score leg overlaps the applicant read.
type fakeTransport struct{}

func (fakeTransport) RoundTrip(*http.Request) (*http.Response, error) {
	time.Sleep(6 * time.Millisecond)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     make(http.Header),
	}, nil
}

// fakeDriver is a minimal database/sql driver that returns canned rows for the
// fixture's queries without a real engine, after a short latency.
type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return fakeConn{}, nil }

type fakeConn struct{}

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (fakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	time.Sleep(6 * time.Millisecond)
	switch {
	case strings.Contains(query, "applicants"):
		return &fakeRows{cols: []string{"name", "income"}, vals: [][]driver.Value{{"Ada", int64(50000)}}}, nil
	case strings.Contains(query, "loans"):
		return &fakeRows{cols: []string{"id", "status"}, vals: [][]driver.Value{{"L1", "approved"}}}, nil
	default:
		return &fakeRows{}, nil
	}
}

func (fakeConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	time.Sleep(2 * time.Millisecond)
	return driver.RowsAffected(1), nil
}

type fakeRows struct {
	cols []string
	vals [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.vals) {
		return io.EOF
	}
	copy(dest, r.vals[r.i])
	r.i++
	return nil
}
