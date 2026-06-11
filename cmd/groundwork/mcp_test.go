package main

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
)

// One scripted MCP session against the obligsvc golden: handshake, discovery,
// a ground call, a fault triage, and the failure modes (unknown tool,
// exceptions without a policy) — all as tool results, never protocol errors.
func TestServeMCPSession(t *testing.T) {
	g, err := graph.LoadFile("../../testdata/groundwork/goldens/obligsvc.graph.json")
	if err != nil {
		t.Fatal(err)
	}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ground","arguments":{"fqn":"example.com/obligsvc/internal/app.DisburseAndCharge"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"triage","arguments":{"frame":"Charge","fail":true}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"exceptions","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"unknown/method"}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"entrypoints","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"fitness","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"reload","arguments":{}}}`,
	}, "\n") + "\n"

	var out strings.Builder
	srv := &mcpServer{path: "../../testdata/groundwork/goldens/obligsvc.graph.json", ix: graph.NewIndex(g)}
	fleet := &mcpFleet{names: []string{srv.path}, services: map[string]*mcpServer{srv.path: srv}}
	if err := serveMCP(strings.NewReader(in), &out, fleet); err != nil {
		t.Fatal(err)
	}

	type resp struct {
		ID     json.RawMessage `json:"id"`
		Result map[string]any  `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	var got []resp
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		var r resp
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("non-JSON line: %q", sc.Text())
		}
		got = append(got, r)
	}
	if len(got) != 10 {
		t.Fatalf("want 10 responses (the notification is silent), got %d", len(got))
	}

	text := func(r resp) string {
		content, _ := r.Result["content"].([]any)
		if len(content) == 0 {
			return ""
		}
		m, _ := content[0].(map[string]any)
		s, _ := m["text"].(string)
		return s
	}
	if pv, _ := got[0].Result["protocolVersion"].(string); pv != "2024-11-05" {
		t.Errorf("initialize protocolVersion = %q", pv)
	}
	if tools, _ := got[1].Result["tools"].([]any); len(tools) != 8 {
		t.Errorf("tools/list = %d tools, want 8", len(tools))
	}
	if !strings.Contains(text(got[2]), "Binding rules") || !strings.Contains(text(got[2]), "partial-effect") {
		t.Errorf("ground card missing binding rules: %q", text(got[2]))
	}
	if !strings.Contains(text(got[3]), "CERTAINLY committed") || !strings.Contains(text(got[3]), "loan.approved") {
		t.Errorf("fault triage missing partial-effect answer: %q", text(got[3]))
	}
	if isErr, _ := got[4].Result["isError"].(bool); !isErr {
		t.Error("unknown tool must be an isError tool result")
	}
	if isErr, _ := got[5].Result["isError"].(bool); !isErr || !strings.Contains(text(got[5]), "--policy") {
		t.Errorf("policy-less exceptions must explain itself: %q", text(got[5]))
	}
	if got[6].Error == nil || got[6].Error.Code != -32601 {
		t.Error("unknown method must be a JSON-RPC method-not-found error")
	}
	if !strings.Contains(text(got[7]), "/transfer") {
		t.Errorf("entrypoints listing missing the route: %q", text(got[7]))
	}
	if isErr, _ := got[8].Result["isError"].(bool); !isErr {
		t.Error("policy-less fitness must be an isError tool result")
	}
	if !strings.Contains(text(got[9]), "reloaded") {
		t.Errorf("reload result: %q", text(got[9]))
	}
}

// One scripted session against a two-service fleet (loansvc + obligsvc): the
// no-service entrypoints listing spans the fleet prefixed by service, the
// fleet-events lens joins loan.approved across both graphs and discloses
// loansvc's dynamically-named publish, per-service tools demand an explicit
// service when several are loaded, and an unknown service is a correctable
// tool error — never a protocol error.
func TestServeMCPFleetSession(t *testing.T) {
	fleet := &mcpFleet{services: map[string]*mcpServer{}}
	for _, name := range []string{"loansvc", "obligsvc"} {
		srv := &mcpServer{path: "../../testdata/groundwork/goldens/" + name + ".graph.json"}
		if err := srv.load(); err != nil {
			t.Fatal(err)
		}
		fleet.services[name] = srv
		fleet.names = append(fleet.names, name)
	}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"entrypoints","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet-events","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ground","arguments":{"fqn":"example.com/obligsvc/internal/app.DisburseAndCharge"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"ground","arguments":{"service":"obligsvc","fqn":"example.com/obligsvc/internal/app.DisburseAndCharge"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"triage","arguments":{"service":"loansvc","event":"payment.settled"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"reach","arguments":{"service":"billing","fqn":"x"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"entrypoints","arguments":{"service":"obligsvc"}}}`,
	}, "\n") + "\n"

	var out strings.Builder
	if err := serveMCP(strings.NewReader(in), &out, fleet); err != nil {
		t.Fatal(err)
	}

	type resp struct {
		Result map[string]any `json:"result"`
	}
	var got []resp
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		var r resp
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("non-JSON line: %q", sc.Text())
		}
		got = append(got, r)
	}
	if len(got) != 7 {
		t.Fatalf("want 7 responses, got %d", len(got))
	}
	text := func(r resp) string {
		content, _ := r.Result["content"].([]any)
		if len(content) == 0 {
			return ""
		}
		m, _ := content[0].(map[string]any)
		s, _ := m["text"].(string)
		return s
	}
	isErr := func(r resp) bool { b, _ := r.Result["isError"].(bool); return b }

	eps := text(got[0])
	for _, want := range []string{"loansvc", "obligsvc", "/transfer", "POST /loan-application", "payment.settled"} {
		if !strings.Contains(eps, want) {
			t.Errorf("fleet entrypoints missing %q:\n%s", want, eps)
		}
	}
	ev := text(got[1])
	for _, want := range []string{
		"loan.approved", "loansvc, obligsvc", // published by both: the cross-service join
		"payment.settled", "consumed by: loansvc",
		"dynamically-named", // loansvc's PUBLISH <dynamic> must be disclosed
	} {
		if !strings.Contains(ev, want) {
			t.Errorf("fleet-events missing %q:\n%s", want, ev)
		}
	}
	if strings.Contains(ev, "obligsvc has") {
		t.Errorf("fleet-events discloses a dynamic effect obligsvc does not have:\n%s", ev)
	}
	if !isErr(got[2]) || !strings.Contains(text(got[2]), "loansvc, obligsvc") {
		t.Errorf("serviceless per-service call must name the loaded services: %q", text(got[2]))
	}
	if isErr(got[3]) || !strings.Contains(text(got[3]), "Binding rules") {
		t.Errorf("ground with service: %q", text(got[3]))
	}
	if isErr(got[4]) || !strings.Contains(text(got[4]), "OnSettled") {
		t.Errorf("triage against loansvc: %q", text(got[4]))
	}
	if !isErr(got[5]) || !strings.Contains(text(got[5]), `unknown service "billing"`) {
		t.Errorf("unknown service must be a correctable tool error: %q", text(got[5]))
	}
	if isErr(got[6]) || !strings.Contains(text(got[6]), "/transfer") || strings.Contains(text(got[6]), "obligsvc ") {
		t.Errorf("scoped entrypoints must be the single-service unprefixed form: %q", text(got[6]))
	}
}

// The transcript is deterministic on purpose (no timestamps): the same
// session produces identical bytes, so the log's shape is locked exactly —
// an init marker per initialize, and per call the raw params plus the
// resolution (answering service, "*" for fleet-wide, absent when resolution
// failed) and the isError outcome. `groundwork transcript` reads this.
func TestMCPTranscriptLog(t *testing.T) {
	srv := &mcpServer{path: "../../testdata/groundwork/goldens/obligsvc.graph.json"}
	if err := srv.load(); err != nil {
		t.Fatal(err)
	}
	fleet := &mcpFleet{names: []string{"oblig"}, services: map[string]*mcpServer{"oblig": srv}}
	var log strings.Builder
	fleet.log = &log
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ground","arguments":{"fqn":"example.com/obligsvc/internal/app.DisburseAndCharge"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fleet-events","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"reach","arguments":{"service":"nope","fqn":"x"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"triage","arguments":{}}}`,
	}, "\n") + "\n"
	var out strings.Builder
	if err := serveMCP(strings.NewReader(in), &out, fleet); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		`{"init":true}`,
		`{"call":{"name":"ground","arguments":{"fqn":"example.com/obligsvc/internal/app.DisburseAndCharge"}},"service":"oblig"}`,
		`{"call":{"name":"fleet-events","arguments":{}},"service":"*"}`,
		`{"call":{"name":"reach","arguments":{"service":"nope","fqn":"x"}},"isError":true}`,
		`{"call":{"name":"triage","arguments":{}},"service":"oblig","isError":true}`,
	}, "\n") + "\n"
	if log.String() != want {
		t.Errorf("transcript bytes:\ngot:\n%swant:\n%s", log.String(), want)
	}
}
