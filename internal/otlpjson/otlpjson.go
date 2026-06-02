// Package otlpjson decodes OTLP/JSON trace exports — the output of an OTel
// Collector file exporter — into flowmap's OTel-free capture model. It is the
// out-of-process analog of the in-process harness's fromOTel adapter
// (harness/harness.go): the single boundary where an external trace
// representation is read, so the canonicalizer and everything downstream
// consume the same stable capture.Span shape and never learn where the spans
// came from (decision D8, post-hoc design [P10.1]).
//
// It deliberately does not depend on gRPC, pdata, or the OTLP proto module —
// those would burden flowmap's public surface. It reads the JSON subset flowmap
// needs and handles the two awkward parts of the format: trace/span ids (left as
// opaque strings, since linkage only needs them to match, never to be parsed,
// which sidesteps the hex-vs-base64 question entirely) and the AnyValue
// attribute union.
package otlpjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jyang234/golang-code-graph/capture"
	"github.com/jyang234/golang-code-graph/ir"
)

// DecodePath decodes a single OTLP/JSON file or, if path is a directory, every
// *.json file within it (sorted), concatenating the spans. Files the collector
// rotates into a directory are read in lexical order.
func DecodePath(path string) ([]capture.Span, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return DecodeFile(path)
	}
	matches, err := filepath.Glob(filepath.Join(path, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []capture.Span
	for _, m := range matches {
		spans, err := DecodeFile(m)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m, err)
		}
		out = append(out, spans...)
	}
	return out, nil
}

// DecodeFile decodes one OTLP/JSON file.
func DecodeFile(path string) ([]capture.Span, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	spans, err := Decode(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return spans, nil
}

// Decode reads OTLP/JSON from r. It accepts the three shapes a collector file
// exporter or otlphttp client may emit: a single ExportTraceServiceRequest
// object, a JSON array of them, or newline-delimited objects (NDJSON) — the
// common rotated-file layout.
func Decode(r io.Reader) ([]capture.Span, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}

	var reqs []exportReq
	if data[0] == '[' {
		if err := json.Unmarshal(data, &reqs); err != nil {
			return nil, fmt.Errorf("otlpjson: %w", err)
		}
	} else {
		// A json.Decoder loop reads successive concatenated values, which covers
		// both a single object and NDJSON without splitting lines by hand.
		dec := json.NewDecoder(bytes.NewReader(data))
		for {
			var req exportReq
			if err := dec.Decode(&req); err == io.EOF {
				break
			} else if err != nil {
				return nil, fmt.Errorf("otlpjson: %w", err)
			}
			reqs = append(reqs, req)
		}
	}

	var out []capture.Span
	for _, req := range reqs {
		for _, rs := range req.ResourceSpans {
			base := attrMap(rs.Resource.Attributes)
			scopes := rs.ScopeSpans
			if len(scopes) == 0 {
				scopes = rs.InstrumentationLibrarySpans // pre-1.0 spelling
			}
			for _, ss := range scopes {
				for _, sp := range ss.Spans {
					out = append(out, toCapture(sp, base))
				}
			}
		}
	}
	return out, nil
}

// toCapture maps one OTLP/JSON span into the internal model, folding the
// resource attributes (service.name, host/pod, etc.) under the span's own
// attributes so downstream can read both; span attributes win on conflict.
func toCapture(sp spanJSON, resource map[string]string) capture.Span {
	attrs := make(map[string]string, len(resource)+len(sp.Attributes))
	for k, v := range resource {
		attrs[k] = v
	}
	for _, kv := range sp.Attributes {
		attrs[kv.Key] = kv.Value.str()
	}
	cs := capture.Span{
		ID:       sp.SpanID,
		ParentID: sp.ParentSpanID,
		Name:     sp.Name,
		Kind:     kindOf(sp.Kind),
		Attrs:    attrs,
		Start:    unixNano(sp.Start),
		End:      unixNano(sp.End),
	}
	switch sp.Status.Code {
	case 1:
		cs.Status = capture.StatusOK
	case 2:
		cs.Status = capture.StatusError
		if et := attrs["error.type"]; et != "" {
			cs.ErrorType = et
		} else {
			cs.ErrorType = "error"
		}
	default:
		cs.Status = capture.StatusUnset
	}
	return cs
}

// kindOf maps the OTLP span-kind enum to flowmap's Kind. Unspecified (0) and any
// unknown value fall back to internal.
func kindOf(k int) ir.Kind {
	switch k {
	case 2:
		return ir.KindServer
	case 3:
		return ir.KindClient
	case 4:
		return ir.KindProducer
	case 5:
		return ir.KindConsumer
	default:
		return ir.KindInternal
	}
}

// unixNano parses an OTLP nanosecond timestamp, tolerating both the proto-JSON
// string encoding ("1700000000000000000") and a bare number. A missing or
// unparseable value yields the zero time — harmless, since post-hoc ordering
// does not rely on caller-clock intervals (post-hoc design [P10.3]).
func unixNano(raw json.RawMessage) time.Time {
	s := strings.Trim(string(raw), `"`)
	if s == "" {
		return time.Time{}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

func attrMap(kvs []keyValue) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = kv.Value.str()
	}
	return m
}

// --- the OTLP/JSON wire shapes (only the fields flowmap reads) ---

type exportReq struct {
	ResourceSpans []resourceSpans `json:"resourceSpans"`
}

type resourceSpans struct {
	Resource                    resourceJSON `json:"resource"`
	ScopeSpans                  []scopeSpans `json:"scopeSpans"`
	InstrumentationLibrarySpans []scopeSpans `json:"instrumentationLibrarySpans"`
}

type resourceJSON struct {
	Attributes []keyValue `json:"attributes"`
}

type scopeSpans struct {
	Spans []spanJSON `json:"spans"`
}

type spanJSON struct {
	SpanID       string          `json:"spanId"`
	ParentSpanID string          `json:"parentSpanId"`
	Name         string          `json:"name"`
	Kind         int             `json:"kind"`
	Start        json.RawMessage `json:"startTimeUnixNano"`
	End          json.RawMessage `json:"endTimeUnixNano"`
	Attributes   []keyValue      `json:"attributes"`
	Status       statusJSON      `json:"status"`
}

type statusJSON struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

// anyValue is the OTLP AnyValue union. flowmap renders every value to a string —
// the salient boundary attributes (http.route, db.system, messaging.destination,
// peer.service) are all strings; the rarer scalar and structured forms are
// stringified so nothing silently becomes empty.
type anyValue struct {
	StringValue *string         `json:"stringValue"`
	BoolValue   *bool           `json:"boolValue"`
	IntValue    json.RawMessage `json:"intValue"`
	DoubleValue *float64        `json:"doubleValue"`
	BytesValue  *string         `json:"bytesValue"`
	ArrayValue  json.RawMessage `json:"arrayValue"`
	KvlistValue json.RawMessage `json:"kvlistValue"`
}

func (v anyValue) str() string {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.BoolValue != nil:
		return strconv.FormatBool(*v.BoolValue)
	case len(v.IntValue) > 0:
		return strings.Trim(string(v.IntValue), `"`) // int64 is JSON-encoded as a string
	case v.DoubleValue != nil:
		return strconv.FormatFloat(*v.DoubleValue, 'g', -1, 64)
	case v.BytesValue != nil:
		return *v.BytesValue
	case len(v.ArrayValue) > 0:
		return string(v.ArrayValue)
	case len(v.KvlistValue) > 0:
		return string(v.KvlistValue)
	}
	return ""
}
