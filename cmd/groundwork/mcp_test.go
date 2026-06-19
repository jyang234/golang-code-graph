package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// One scripted MCP session against the obligsvc golden: handshake, discovery,
// a ground call, a fault triage, and the failure modes (unknown tool,
// exceptions without a policy, an over-specified triage) — all as tool
// results, never protocol errors.
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
		`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"triage","arguments":{"frame":"Charge","event":"loan.approved"}}}`,
	}, "\n") + "\n"

	var out strings.Builder
	srv := &mcpServer{path: "../../testdata/groundwork/goldens/obligsvc.graph.json", ix: graph.NewIndex(g)}
	fleet := newMCPFleet(map[string]*mcpServer{srv.path: srv})
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
	if len(got) != 11 {
		t.Fatalf("want 11 responses (the notification is silent), got %d", len(got))
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
	if tools, _ := got[1].Result["tools"].([]any); len(tools) != 11 {
		t.Errorf("tools/list = %d tools, want 11", len(tools))
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
	// Two symptoms must be rejected, mirroring the CLI: a silently-ignored
	// second symptom would mis-scope an incident hunt.
	if isErr, _ := got[10].Result["isError"].(bool); !isErr || !strings.Contains(text(got[10]), "exactly one") {
		t.Errorf("two-symptom triage must be an isError tool result naming the rule: %q", text(got[10]))
	}
}

// One scripted session against a two-service fleet (loansvc + obligsvc): the
// no-service entrypoints listing spans the fleet prefixed by service, the
// fleet-events lens joins loan.approved across both graphs and discloses
// loansvc's dynamically-named publish, per-service tools demand an explicit
// service when several are loaded, and an unknown service is a correctable
// tool error — never a protocol error.
func TestServeMCPFleetSession(t *testing.T) {
	services := map[string]*mcpServer{}
	for _, name := range []string{"loansvc", "obligsvc"} {
		srv := &mcpServer{path: "../../testdata/groundwork/goldens/" + name + ".graph.json"}
		if err := srv.load(); err != nil {
			t.Fatal(err)
		}
		services[name] = srv
	}
	// The broker guarantee is fleet-wide; attach it to one loaded service so the
	// chains lens prints the assumed link.
	p, err := policy.Load("../../testdata/groundwork/policies/bus-brokers.json")
	if err != nil {
		t.Fatal(err)
	}
	services["obligsvc"].p = p
	fleet := newMCPFleet(services)
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"entrypoints","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet-events","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ground","arguments":{"fqn":"example.com/obligsvc/internal/app.DisburseAndCharge"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"ground","arguments":{"service":"obligsvc","fqn":"example.com/obligsvc/internal/app.DisburseAndCharge"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"triage","arguments":{"service":"loansvc","event":"payment.settled"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"reach","arguments":{"service":"billing","fqn":"x"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"entrypoints","arguments":{"service":"obligsvc"}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"chains","arguments":{}}}`,
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
	if len(got) != 8 {
		t.Fatalf("want 8 responses, got %d", len(got))
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
	ch := text(got[7])
	for _, want := range []string{
		"chain: loan.approved",
		"[proven] producer — obligsvc",
		"[assumed] broker — bus",
		"UNSIGNED",               // the attached block carries no warrant
		"open downstream",        // no consumer of loan.approved in this fleet
		"chain: payment.settled", // consumed by loansvc
	} {
		if !strings.Contains(ch, want) {
			t.Errorf("chains lens missing %q:\n%s", want, ch)
		}
	}
}

// The transcript is deterministic on purpose (no timestamps, sequential
// session ids): the same session produces identical bytes, so the log's
// shape is locked exactly — an id-stamped init line per initialize, and per
// call the raw params plus the resolution (answering service, "*" for
// fleet-wide, absent when resolution failed), the session id, and the
// isError outcome. `groundwork transcript` reads this.
func TestMCPTranscriptLog(t *testing.T) {
	srv := &mcpServer{path: "../../testdata/groundwork/goldens/obligsvc.graph.json"}
	if err := srv.load(); err != nil {
		t.Fatal(err)
	}
	fleet := newMCPFleet(map[string]*mcpServer{"oblig": srv})
	var log strings.Builder
	fleet.log = &log
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ground","arguments":{"fqn":"example.com/obligsvc/internal/app.DisburseAndCharge"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fleet-events","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"reach","arguments":{"service":"nope","fqn":"x"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"triage","arguments":{}}}`,
	}, "\n") + "\n"
	var out strings.Builder
	if err := serveMCP(strings.NewReader(in), &out, fleet); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		`{"init":true,"session":"1"}`,
		`{"call":{"name":"ground","arguments":{"fqn":"example.com/obligsvc/internal/app.DisburseAndCharge"}},"service":"oblig","session":"1"}`,
		`{"call":{"name":"fleet-events","arguments":{}},"service":"*","session":"1"}`,
		`{"call":{"name":"reach","arguments":{"service":"nope","fqn":"x"}},"session":"1","isError":true}`,
		`{"init":true,"session":"2"}`,
		`{"call":{"name":"triage","arguments":{}},"service":"oblig","session":"2","isError":true}`,
	}, "\n") + "\n"
	if log.String() != want {
		t.Errorf("transcript bytes:\ngot:\n%swant:\n%s", log.String(), want)
	}
}

// Service names label transcript lines, listings, and error texts, so the
// charset is tight: the transcript's sentinels ("*") and list separators
// must be unreachable by configuration.
func TestValidServiceName(t *testing.T) {
	for name, want := range map[string]bool{
		"payments": true, "pay_ments-2.x": true,
		"": false, "*": false, "a b": false, "a,b": false,
		"(fleet-wide)": false, "a=b": false, "a/b": false,
	} {
		if got := validServiceName(name); got != want {
			t.Errorf("validServiceName(%q) = %v, want %v", name, got, want)
		}
	}
}

// A stale fleet with no entrypoints must answer with BOTH disclosures: the
// staleness warning and the empty-case explanation — one must never swallow
// the other.
func TestFleetEntrypointsStaleAndEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bare.graph.json")
	if err := os.WriteFile(path, []byte(`{"nodes":[],"edges":[],"blind_spots":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &mcpServer{path: path}
	if err := a.load(); err != nil {
		t.Fatal(err)
	}
	b := &mcpServer{path: path}
	if err := b.load(); err != nil {
		t.Fatal(err)
	}
	fleet := newMCPFleet(map[string]*mcpServer{"a": a, "b": b})
	a.mtime = a.mtime.Add(-1) // simulate the file having changed since load
	res := fleet.fleetEntrypoints()
	text := res["content"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(text, "service a") || !strings.Contains(text, "changed on disk") {
		t.Errorf("missing the stale disclosure: %q", text)
	}
	if !strings.Contains(text, "no named entrypoints") {
		t.Errorf("stale note swallowed the empty-case explanation: %q", text)
	}
}
