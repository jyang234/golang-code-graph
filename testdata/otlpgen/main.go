// Command otlpgen emits an authoritative OTLP/JSON sample of the loansvc flow,
// marshaled with the OTel Collector's own ptrace.JSONMarshaler — the exact
// encoder the collector `file` exporter (format: json) uses. Running it
// regenerates testdata/otlp/loansvc.collector.otlp.json, the real-format sample
// the otlpjson decoder is validated against, so flowmap's reader is pinned to
// collector output rather than a hand-authored guess — without needing any real
// trace store or proprietary data.
//
// It is a standalone module (own go.mod, deliberately NOT in go.work): the heavy
// pdata dependency stays entirely out of the engine's module graph and off the
// public harness/flow/ir surface. Regenerate with:
//
//	cd testdata/otlpgen && GOWORK=off go run . > ../otlp/loansvc.collector.otlp.json
package main

import (
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

func main() {
	traces := ptrace.NewTraces()
	rs := traces.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "loansvc")
	rs.Resource().Attributes().PutStr("host.name", "pod-7f9c") // resource noise the allowlist must drop
	ss := rs.ScopeSpans().AppendEmpty()
	ss.Scope().SetName("loansvc")

	base := time.Unix(1700000000, 0).UTC()
	var nextID byte = 1
	spanID := func() pcommon.SpanID {
		id := pcommon.SpanID{0, 0, 0, 0, 0, 0, 0, nextID}
		nextID++
		return id
	}
	traceID := pcommon.TraceID{0x5b, 0x8e, 0xff, 0xf7, 0x98, 0x03, 0x81, 0x03, 0xd2, 0x69, 0xb6, 0x33, 0x81, 0x3f, 0xc6, 0x0c}

	type span struct {
		id, parent pcommon.SpanID
		name       string
		kind       ptrace.SpanKind
		startMS    int
		status     ptrace.StatusCode
		attrs      map[string]any
	}

	root := spanID()
	root00 := pcommon.SpanID{} // empty => the inbound entry's caller is outside this capture
	mk := func(name string, kind ptrace.SpanKind, parent pcommon.SpanID, startMS int, status ptrace.StatusCode, attrs map[string]any) span {
		return span{id: spanID(), parent: parent, name: name, kind: kind, startMS: startMS, status: status, attrs: attrs}
	}

	spans := []span{
		{id: root, parent: root00, name: "POST /loan-application", kind: ptrace.SpanKindServer, startMS: 0, status: ptrace.StatusCodeOk,
			attrs: map[string]any{"http.request.method": "POST", "http.route": "/loan-application", "http.response.status_code": int64(200)}},
		mk("query applicants", ptrace.SpanKindClient, root, 5, ptrace.StatusCodeUnset,
			map[string]any{"db.system": "postgresql", "db.statement": "SELECT name, income FROM applicants WHERE id = $1"}),
		mk("GET", ptrace.SpanKindClient, root, 6, ptrace.StatusCodeOk,
			map[string]any{"http.request.method": "GET", "peer.service": "credit-bureau", "http.target": "/score/8412"}),
		mk("charge", ptrace.SpanKindClient, root, 20, ptrace.StatusCodeOk,
			map[string]any{"http.request.method": "POST", "peer.service": "payment-gw", "http.target": "/charge/8412"}),
		mk("loan.approved send", ptrace.SpanKindProducer, root, 25, ptrace.StatusCodeOk,
			map[string]any{"messaging.destination.name": "loan.approved", "messaging.operation": "publish"}),
		mk("ledger insert", ptrace.SpanKindClient, root, 28, ptrace.StatusCodeUnset,
			map[string]any{"db.system": "postgres", "db.statement": "INSERT INTO ledger (loan_id, amount) VALUES ($1, $2)"}),
		mk("audit insert", ptrace.SpanKindClient, root, 32, ptrace.StatusCodeUnset,
			map[string]any{"db.system": "postgres", "db.statement": "INSERT INTO audit_log (loan_id) VALUES ($1)"}),
	}

	for _, s := range spans {
		sp := ss.Spans().AppendEmpty()
		sp.SetTraceID(traceID)
		sp.SetSpanID(s.id)
		if s.parent != root00 {
			sp.SetParentSpanID(s.parent)
		}
		sp.SetName(s.name)
		sp.SetKind(s.kind)
		sp.SetStartTimestamp(pcommon.NewTimestampFromTime(base.Add(time.Duration(s.startMS) * time.Millisecond)))
		sp.SetEndTimestamp(pcommon.NewTimestampFromTime(base.Add(time.Duration(s.startMS+2) * time.Millisecond)))
		sp.Status().SetCode(s.status)
		// flowmap.flow is the per-flow tag a baggagecopy span processor promotes
		// onto every span out of process.
		sp.Attributes().PutStr("flowmap.flow", "loan-application")
		for k, v := range s.attrs {
			switch t := v.(type) {
			case string:
				sp.Attributes().PutStr(k, t)
			case int64:
				sp.Attributes().PutInt(k, t)
			default:
				panic(fmt.Sprintf("unsupported attr type for %s", k))
			}
		}
	}

	b, err := (&ptrace.JSONMarshaler{}).MarshalTraces(traces)
	if err != nil {
		fmt.Fprintln(os.Stderr, "otlpgen:", err)
		os.Exit(1)
	}
	os.Stdout.Write(append(b, '\n'))
}
