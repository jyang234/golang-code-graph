package ingest_test

import (
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jyang234/golang-code-graph/capture"
	"github.com/jyang234/golang-code-graph/harness"
	"github.com/jyang234/golang-code-graph/internal/canon"
	"github.com/jyang234/golang-code-graph/internal/ingest"
	"github.com/jyang234/golang-code-graph/internal/loansut"
	"github.com/jyang234/golang-code-graph/internal/otlpjson"
	"github.com/jyang234/golang-code-graph/ir"
)

// TestDogfoodLoansvcPostHoc is the end-to-end dogfood: it drives flowmap's own
// loansut fixture through the in-process harness to obtain the spans the service
// really emits, serializes them to OTLP/JSON exactly as a collector file
// exporter would, then runs that wire format back through the post-hoc pipeline
// (otlpjson.Decode → ingest.Group → canon). The boundary-effect set it recovers
// must equal the one the committed in-process golden asserts — proving the
// out-of-process path reconstructs the same contract from the same service,
// across the JSON wire, with no in-process signals (goroutine ids, live
// recorder) available.
func TestDogfoodLoansvcPostHoc(t *testing.T) {
	// 1. Drive the real fixture in-process; capture the spans it actually emits.
	app := harness.NewInProcess(t, loansut.Handler(loansut.Options{}), harness.WithService("loansvc"))
	cf, err := app.HTTP("POST", "/loan-application", nil).Capture(harness.CaptureOptions{
		Markers: []string{
			"HTTP POST payment-gw /charge/{id}",
			"PUBLISH loan.approved",
			"DB postgres INSERT ledger",
			"DB postgres INSERT audit_log",
		},
		Quiet:   10 * time.Millisecond,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("in-process capture: %v", err)
	}

	// 2. Serialize to OTLP/JSON as the collector would, tagging each span with the
	// flowmap.flow slug (the out-of-process selector the baggagecopy processor
	// promotes onto spans).
	wire := toOTLPJSON(cf, "post-loan-application")

	// 3. Run the wire format back through the post-hoc pipeline.
	spans, err := otlpjson.Decode(strings.NewReader(string(wire)))
	if err != nil {
		t.Fatalf("otlpjson decode: %v", err)
	}
	flows := ingest.Group(spans)
	if len(flows) != 1 {
		t.Fatalf("got %d fragments, want 1 (single service)", len(flows))
	}
	if flows[0].Synthesized {
		t.Errorf("the inbound server span should root the fragment, not a synthesized node")
	}
	tr, err := canon.Canonicalize(flows[0].Flow, nil)
	if err != nil {
		t.Fatalf("post-hoc canonicalize: %v", err)
	}

	// 4. The post-hoc boundary-effect set must equal the committed in-process
	// golden's (design D-PH3: the gate is the boundary-effect set).
	got := boundaryEffects(tr.Root)
	golden, err := ir.Load(mustRead(t, "../../flow/testdata/flows/post_loan_application.golden.json"))
	if err != nil {
		t.Fatalf("load in-process golden: %v", err)
	}
	want := boundaryEffects(golden.Root)

	if !equalSets(got, want) {
		t.Errorf("post-hoc boundary effects differ from the in-process golden:\n  post-hoc: %v\n  in-proc:  %v", got, want)
	}
}

// boundaryEffects returns the sorted set of canonical op keys naming a boundary
// effect (published/consumed event or outbound HTTP/RPC dependency).
func boundaryEffects(s *ir.CanonicalSpan) []string {
	set := map[string]bool{}
	var walk func(*ir.CanonicalSpan)
	walk = func(n *ir.CanonicalSpan) {
		if n == nil {
			return
		}
		for _, p := range []string{"PUBLISH ", "CONSUME ", "HTTP ", "RPC "} {
			if strings.HasPrefix(n.Op, p) {
				set[n.Op] = true
			}
		}
		for _, g := range n.Children {
			for _, m := range g.Members {
				walk(m)
			}
		}
	}
	walk(s)
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// toOTLPJSON renders a captured flow into the OTLP/JSON shape a collector file
// exporter emits: one resourceSpans (service.name from the flow), one scopeSpans,
// each span carrying the flowmap.flow tag. It is the inverse of otlpjson.Decode,
// used only to dogfood the wire round-trip against real fixture spans.
func toOTLPJSON(cf *capture.CapturedFlow, slug string) []byte {
	type kv = map[string]any
	attrsOf := func(s capture.Span) []kv {
		out := []kv{{"key": "flowmap.flow", "value": kv{"stringValue": slug}}}
		keys := make([]string, 0, len(s.Attrs))
		for k := range s.Attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, kv{"key": k, "value": kv{"stringValue": s.Attrs[k]}})
		}
		return out
	}
	statusOf := func(s capture.Span) kv {
		switch s.Status {
		case capture.StatusOK:
			return kv{"code": 1}
		case capture.StatusError:
			return kv{"code": 2, "message": s.ErrorType}
		default:
			return kv{"code": 0}
		}
	}

	spans := make([]kv, 0, len(cf.Spans))
	for _, s := range cf.Spans {
		spans = append(spans, kv{
			"spanId":            s.ID,
			"parentSpanId":      s.ParentID,
			"name":              s.Name,
			"kind":              otlpKind(s.Kind),
			"startTimeUnixNano": strconv.FormatInt(s.Start.UnixNano(), 10), // string, as proto-JSON encodes int64
			"endTimeUnixNano":   strconv.FormatInt(s.End.UnixNano(), 10),
			"attributes":        attrsOf(s),
			"status":            statusOf(s),
		})
	}

	doc := kv{"resourceSpans": []kv{{
		"resource":   kv{"attributes": []kv{{"key": "service.name", "value": kv{"stringValue": cf.Service}}}},
		"scopeSpans": []kv{{"spans": spans}},
	}}}
	b, _ := json.Marshal(doc)
	return b
}

func otlpKind(k ir.Kind) int {
	switch k {
	case ir.KindServer:
		return 2
	case ir.KindClient:
		return 3
	case ir.KindProducer:
		return 4
	case ir.KindConsumer:
		return 5
	default:
		return 1
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
