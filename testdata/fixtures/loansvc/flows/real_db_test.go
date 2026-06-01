package flows_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	_ "modernc.org/sqlite"

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

// schema is the minimal SQLite schema the loan-application flow exercises. The
// store's statements are written with Postgres `$1` placeholders, which SQLite
// also binds positionally, so the real instrumented store runs unchanged — this
// is the harness spec's "SQLite standing in for Postgres" fallback (§5).
const schema = `
CREATE TABLE applicants (id TEXT PRIMARY KEY, name TEXT, income INTEGER);
CREATE TABLE loans      (id TEXT PRIMARY KEY, status TEXT);
CREATE TABLE ledger     (loan_id TEXT, amount INTEGER);
CREATE TABLE audit_log  (loan_id TEXT);
INSERT INTO applicants (id, name, income) VALUES ('A1', 'Ada', 50000);
`

// wireRealDB builds the loansvc service over a real in-memory SQLite database
// (real query execution, real rows) plus the fake outbound HTTP transport.
func wireRealDB(t *testing.T) http.Handler {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// One connection so the in-memory database persists across queries.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	loans := store.New(db)
	c := client.NewWithClient(&http.Client{Transport: fakeTransport{}})
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
	return mux
}

// TestLoanApplicationFlowRealDB drives the same flow against a real SQLite
// database and asserts it against the SAME committed golden as the fake-driver
// run. Identical canonical IR proves two things at once: the behavioral pipeline
// faithfully captures real DB execution, and the fake driver used elsewhere is a
// faithful stand-in (the captured db.statement/op are a function of the
// instrumented code, not the backing engine).
func TestLoanApplicationFlowRealDB(t *testing.T) {
	app := harness.NewInProcess(t, wireRealDB(t), harness.WithService("loansvc"))

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
