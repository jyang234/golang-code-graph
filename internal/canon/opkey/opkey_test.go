package opkey

import (
	"testing"

	"github.com/jyang234/golang-code-graph/ir"
)

func TestServerKey(t *testing.T) {
	op, peer := Of(ir.KindServer, map[string]string{
		"http.request.method": "POST",
		"http.route":          "/loan-application",
	}, "POST /loan-application")
	if op != "HTTP POST /loan-application" {
		t.Errorf("op = %q", op)
	}
	if peer != "" {
		t.Errorf("server peer = %q, want empty", peer)
	}
}

func TestClientHTTPKey(t *testing.T) {
	op, peer := Of(ir.KindClient, map[string]string{
		"http.request.method": "GET",
		"http.route":          "/score/{id}",
		"peer.service":        "credit-bureau",
	}, "GET /score/8412")
	if op != "HTTP GET credit-bureau /score/{id}" {
		t.Errorf("op = %q", op)
	}
	if peer != "credit-bureau" {
		t.Errorf("peer = %q", peer)
	}
}

func TestClientHTTPKeyParameterizesRawPath(t *testing.T) {
	// No http.route attribute — only a raw target. The key must still template it.
	op, _ := Of(ir.KindClient, map[string]string{
		"http.method":  "GET",
		"http.target":  "/score/8412",
		"peer.service": "credit-bureau",
	}, "")
	if op != "HTTP GET credit-bureau /score/{id}" {
		t.Errorf("op = %q, want templated", op)
	}
}

func TestDBKey(t *testing.T) {
	op, peer := Of(ir.KindClient, map[string]string{
		"db.system":    "postgresql",
		"db.statement": "SELECT name, income FROM applicants WHERE id = $1",
	}, "SELECT applicants")
	if op != "DB postgresql SELECT applicants" {
		t.Errorf("op = %q", op)
	}
	if peer != "postgresql" {
		t.Errorf("peer = %q", peer)
	}
}

func TestDBKeyInsert(t *testing.T) {
	op, _ := Of(ir.KindClient, map[string]string{
		"db.system":    "postgres",
		"db.statement": "INSERT INTO ledger (loan_id, amount) VALUES ($1, $2)",
	}, "")
	if op != "DB postgres INSERT ledger" {
		t.Errorf("op = %q, want 'DB postgres INSERT ledger'", op)
	}
}

func TestPublishKey(t *testing.T) {
	op, peer := Of(ir.KindProducer, map[string]string{
		"messaging.destination.name": "loan.approved",
	}, "")
	if op != "PUBLISH loan.approved" {
		t.Errorf("op = %q", op)
	}
	if peer != "Bus" {
		t.Errorf("peer = %q, want Bus", peer)
	}
}

func TestConsumeKey(t *testing.T) {
	op, peer := Of(ir.KindConsumer, map[string]string{
		"messaging.destination": "payment.settled",
	}, "")
	if op != "CONSUME payment.settled" || peer != "Bus" {
		t.Errorf("op=%q peer=%q", op, peer)
	}
}

func TestRPCKey(t *testing.T) {
	op, peer := Of(ir.KindClient, map[string]string{
		"rpc.system":  "grpc",
		"rpc.service": "LedgerService",
		"rpc.method":  "PostEntry",
	}, "")
	if op != "RPC LedgerService/PostEntry" {
		t.Errorf("op = %q", op)
	}
	if peer != "LedgerService" {
		t.Errorf("peer = %q", peer)
	}
}

func TestInternalKeyFallsBackToName(t *testing.T) {
	op, peer := Of(ir.KindInternal, nil, "evaluateApplication")
	if op != "evaluateApplication" || peer != "" {
		t.Errorf("op=%q peer=%q", op, peer)
	}
}
