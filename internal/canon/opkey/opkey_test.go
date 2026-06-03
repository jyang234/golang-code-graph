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

// TestAWSMessagingFromClientSpan: the AWS SDK models SNS/SQS as CLIENT-kind RPC
// spans that still carry messaging.* attributes. The destination and operation
// must drive the key (and the effective kind), not the RPC/HTTP fallback — an
// SNS publish is PUBLISH topic, not a bare call to "sns".
func TestAWSMessagingFromClientSpan(t *testing.T) {
	cases := []struct {
		name     string
		attrs    map[string]string
		wantOp   string
		wantKind ir.Kind
	}{
		{
			name: "sns publish",
			attrs: map[string]string{
				"rpc.system": "aws-api", "rpc.service": "SNS", "rpc.method": "Publish",
				"messaging.system": "aws_sns", "messaging.destination.name": "loan-events",
				"messaging.operation": "publish",
			},
			wantOp: "PUBLISH loan-events", wantKind: ir.KindProducer,
		},
		{
			name: "sqs receive",
			attrs: map[string]string{
				"rpc.system": "aws-api", "rpc.service": "SQS", "rpc.method": "ReceiveMessage",
				"messaging.system": "aws_sqs", "messaging.destination.name": "loan-queue",
				"messaging.operation": "receive",
			},
			wantOp: "CONSUME loan-queue", wantKind: ir.KindConsumer,
		},
		{
			name: "sqs delete is settle, not a second receive",
			attrs: map[string]string{
				"rpc.system": "aws-api", "rpc.service": "SQS", "rpc.method": "DeleteMessage",
				"messaging.system": "aws_sqs", "messaging.destination.name": "loan-queue",
				"messaging.operation": "delete",
			},
			wantOp: "SETTLE loan-queue", wantKind: ir.KindProducer,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op, peer := Of(ir.KindClient, c.attrs, "")
			if op != c.wantOp {
				t.Errorf("op = %q, want %q", op, c.wantOp)
			}
			if peer != "Bus" {
				t.Errorf("peer = %q, want Bus", peer)
			}
			if k := EffectiveKind(ir.KindClient, c.attrs); k != c.wantKind {
				t.Errorf("EffectiveKind = %v, want %v", k, c.wantKind)
			}
		})
	}
}

// TestAWSSQSWithoutMessagingAttrs reflects the real AWS-SDK SQS shape: the spans
// carry no messaging.destination.name and no messaging.operation — only
// rpc.system.name + rpc.method + the HTTP-to-endpoint transport. The rpc.method
// must discriminate receive from delete, the peer must be the AWS service (not
// the LocalStack transport host), and neither must fall through to bare HTTP.
func TestAWSSQSWithoutMessagingAttrs(t *testing.T) {
	recv := map[string]string{
		"rpc.system.name": "aws-api", "rpc.method": "SQS/ReceiveMessage",
		"messaging.system":    "aws_sqs",
		"http.request.method": "POST", "url.full": "http://floci:4566/000000000000/q",
		"server.address": "floci",
	}
	del := map[string]string{
		"rpc.system.name": "aws-api", "rpc.method": "SQS/DeleteMessage",
		"messaging.system":    "aws_sqs",
		"http.request.method": "POST", "url.full": "http://floci:4566/000000000000/q",
		"server.address": "floci",
	}
	rOp, rPeer := Of(ir.KindClient, recv, "")
	dOp, dPeer := Of(ir.KindClient, del, "")
	if rOp != "RPC SQS/ReceiveMessage" || dOp != "RPC SQS/DeleteMessage" {
		t.Errorf("ops = %q / %q, want RPC SQS/ReceiveMessage / RPC SQS/DeleteMessage", rOp, dOp)
	}
	if rOp == dOp {
		t.Error("receive and delete merged")
	}
	if rPeer != "SQS" || dPeer != "SQS" {
		t.Errorf("peer = %q / %q, want SQS (the AWS service, not the transport host floci)", rPeer, dPeer)
	}
}

// TestReceiveAndSettleDoNotMerge guards defect #3 directly: receiving a message
// and acknowledging it are distinct ops over the same queue.
func TestReceiveAndSettleDoNotMerge(t *testing.T) {
	recv, _ := Of(ir.KindClient, map[string]string{
		"messaging.destination.name": "q", "messaging.operation": "receive"}, "")
	ack, _ := Of(ir.KindClient, map[string]string{
		"messaging.destination.name": "q", "messaging.operation": "settle"}, "")
	if recv == ack {
		t.Fatalf("receive and settle collapsed into the same op %q", recv)
	}
	if recv != "CONSUME q" || ack != "SETTLE q" {
		t.Errorf("recv=%q ack=%q, want CONSUME q / SETTLE q", recv, ack)
	}
}

// TestRPCSystemNameSpelling: the newer rpc.system.name attribute must select the
// RPC path just as rpc.system does, so an RPC call carrying only the new spelling
// is not misclassified as a bare HTTP call.
func TestRPCSystemNameSpelling(t *testing.T) {
	op, peer := Of(ir.KindClient, map[string]string{
		"rpc.system.name": "grpc",
		"rpc.service":     "LedgerService",
		"rpc.method":      "PostEntry",
	}, "")
	if op != "RPC LedgerService/PostEntry" {
		t.Errorf("op = %q, want RPC LedgerService/PostEntry", op)
	}
	if peer != "LedgerService" {
		t.Errorf("peer = %q, want LedgerService", peer)
	}
}

// TestEffectiveKindPassThrough: a non-messaging span keeps its raw kind, so the
// normalization is inert outside broker interactions.
func TestEffectiveKindPassThrough(t *testing.T) {
	http := map[string]string{"http.request.method": "GET", "peer.service": "p"}
	if k := EffectiveKind(ir.KindClient, http); k != ir.KindClient {
		t.Errorf("non-messaging client kind = %v, want client", k)
	}
	if k := EffectiveKind(ir.KindServer, map[string]string{"http.route": "/x"}); k != ir.KindServer {
		t.Errorf("server kind = %v, want server", k)
	}
}

func TestInternalKeyFallsBackToName(t *testing.T) {
	op, peer := Of(ir.KindInternal, nil, "evaluateApplication")
	if op != "evaluateApplication" || peer != "" {
		t.Errorf("op=%q peer=%q", op, peer)
	}
}

// TestPrefixConstantsMatchOf pins the exported prefix constants to Of's actual
// output, so a change to the rendered grammar can't silently desync a consumer
// (the system-context graph, coverage) that matches on the constants.
func TestPrefixConstantsMatchOf(t *testing.T) {
	cases := []struct {
		kind   ir.Kind
		attrs  map[string]string
		prefix string
	}{
		{ir.KindProducer, map[string]string{"messaging.destination.name": "e"}, PublishPrefix},
		{ir.KindConsumer, map[string]string{"messaging.destination.name": "e"}, ConsumePrefix},
		{ir.KindClient, map[string]string{"db.system": "postgres", "db.statement": "SELECT 1 FROM t"}, DBPrefix},
		{ir.KindClient, map[string]string{"rpc.service": "S", "rpc.method": "M"}, RPCPrefix},
		{ir.KindClient, map[string]string{"http.request.method": "GET", "peer.service": "p", "http.route": "/x"}, HTTPPrefix},
		{ir.KindServer, map[string]string{"http.request.method": "GET", "http.route": "/x"}, HTTPPrefix},
	}
	for _, c := range cases {
		op, _ := Of(c.kind, c.attrs, "name")
		if len(op) < len(c.prefix) || op[:len(c.prefix)] != c.prefix {
			t.Errorf("Of(%s)=%q, want prefix %q", c.kind, op, c.prefix)
		}
	}
}
