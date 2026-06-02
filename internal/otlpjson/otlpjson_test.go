package otlpjson

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/capture"
	"github.com/jyang234/golang-code-graph/ir"
)

const fixture = "../../testdata/otlp/loan-application.otlp.json"

func TestDecodeFixture(t *testing.T) {
	spans, err := DecodeFile(fixture)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(spans) != 4 {
		t.Fatalf("got %d spans, want 4", len(spans))
	}

	byID := map[string]capture.Span{}
	for _, s := range spans {
		byID[s.ID] = s
	}

	server := byID["0000000000000001"]
	if server.Kind != ir.KindServer {
		t.Errorf("server span kind = %q, want server", server.Kind)
	}
	if server.ParentID != "" {
		t.Errorf("server span parent = %q, want empty (root)", server.ParentID)
	}
	// resource attribute folded onto the span.
	if got := server.Attr("service.name"); got != "loansvc" {
		t.Errorf("service.name = %q, want loansvc", got)
	}
	// the promoted baggage tag.
	if got := server.Attr("flowmap.flow"); got != "loan-application" {
		t.Errorf("flowmap.flow = %q, want loan-application", got)
	}
	if server.Status != capture.StatusOK {
		t.Errorf("server status = %q, want ok", server.Status)
	}

	// the producer span carries the messaging destination.
	pub := byID["0000000000000004"]
	if pub.Kind != ir.KindProducer {
		t.Errorf("publish span kind = %q, want producer", pub.Kind)
	}
	if got := pub.Attr("messaging.destination.name"); got != "loan.approved" {
		t.Errorf("destination = %q, want loan.approved", got)
	}

	// parent linkage resolves entirely within the decoded set.
	for _, s := range spans {
		if s.ParentID != "" {
			if _, ok := byID[s.ParentID]; !ok {
				t.Errorf("span %s has dangling parent %s", s.ID, s.ParentID)
			}
		}
	}
}

// TestDecodeFormats covers the three layouts a collector / exporter may emit:
// a single object, a JSON array, and NDJSON — all must parse to the same spans.
func TestDecodeFormats(t *testing.T) {
	obj := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"svc"}}]},
		"scopeSpans":[{"spans":[{"spanId":"01","name":"a","kind":2,"attributes":[],"status":{"code":1}}]}]}]}`
	array := "[" + obj + "," + obj + "]"
	ndjson := obj + "\n" + obj + "\n"

	cases := map[string]struct {
		in   string
		want int
	}{
		"single object": {obj, 1},
		"array":         {array, 2},
		"ndjson":        {ndjson, 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			spans, err := Decode(strings.NewReader(tc.in))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(spans) != tc.want {
				t.Fatalf("got %d spans, want %d", len(spans), tc.want)
			}
			if spans[0].Attr("service.name") != "svc" {
				t.Errorf("resource attr not folded onto span: %v", spans[0].Attrs)
			}
		})
	}
}

// TestAnyValueUnion exercises the scalar AnyValue forms flowmap stringifies, plus
// the int64-as-string proto-JSON quirk.
func TestAnyValueUnion(t *testing.T) {
	in := `{"resourceSpans":[{"scopeSpans":[{"spans":[{"spanId":"01","name":"a","kind":1,
		"attributes":[
			{"key":"s","value":{"stringValue":"x"}},
			{"key":"b","value":{"boolValue":true}},
			{"key":"i","value":{"intValue":"42"}},
			{"key":"d","value":{"doubleValue":1.5}}
		],"status":{"code":0}}]}]}]}`
	spans, err := Decode(strings.NewReader(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	a := spans[0].Attrs
	for k, want := range map[string]string{"s": "x", "b": "true", "i": "42", "d": "1.5"} {
		if a[k] != want {
			t.Errorf("attr %q = %q, want %q", k, a[k], want)
		}
	}
}

// TestDecodeEmpty tolerates an empty file (a collector that flushed nothing).
func TestDecodeEmpty(t *testing.T) {
	spans, err := Decode(strings.NewReader("  \n"))
	if err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if len(spans) != 0 {
		t.Fatalf("got %d spans, want 0", len(spans))
	}
}
