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
	if err := serveMCP(strings.NewReader(in), &out, srv); err != nil {
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
	if tools, _ := got[1].Result["tools"].([]any); len(tools) != 7 {
		t.Errorf("tools/list = %d tools, want 7", len(tools))
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
