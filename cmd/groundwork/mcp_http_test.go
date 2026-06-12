package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// One session against the streamable-HTTP front door: the front door itself
// (bearer auth, Origin rejection, method and batch discipline, notification
// acknowledgement) is HTTP; everything behind it is the same dispatch the
// stdio transport uses, answering with the same tool results.
func TestServeMCPHTTPSession(t *testing.T) {
	srv := &mcpServer{path: "../../testdata/groundwork/goldens/obligsvc.graph.json"}
	if err := srv.load(); err != nil {
		t.Fatal(err)
	}
	fleet := newMCPFleet(map[string]*mcpServer{"obligsvc": srv})
	fleet.proto = "2025-03-26"
	var log strings.Builder
	fleet.log = &log
	ts := httptest.NewServer(fleet.httpHandler("s3cret"))
	defer ts.Close()

	post := func(body, token, origin, session string) (*http.Response, map[string]any) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var decoded map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&decoded)
		return resp, decoded
	}

	resp, body := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "s3cret", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("initialize status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	result, _ := body["result"].(map[string]any)
	if pv, _ := result["protocolVersion"].(string); pv != "2025-03-26" {
		t.Errorf("HTTP transport must report the streamable-HTTP revision, got %q", pv)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize must return an Mcp-Session-Id (the transcript attribution label)")
	}

	resp, body = post(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ground","arguments":{"fqn":"example.com/obligsvc/internal/app.DisburseAndCharge"}}}`, "s3cret", "", sid)
	result, _ = body["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if resp.StatusCode != 200 || len(content) == 0 {
		t.Fatalf("ground over HTTP: status %d, body %v", resp.StatusCode, body)
	}
	if text, _ := content[0].(map[string]any)["text"].(string); !strings.Contains(text, "Binding rules") {
		t.Errorf("ground card over HTTP missing binding rules: %q", text)
	}
	// The shared team log attributes by session id, not line order: the
	// init line carries the minted id, and the echoed header labels the call.
	if got := log.String(); !strings.Contains(got, `{"init":true,"session":"`+sid+`"}`) ||
		!strings.Contains(got, `"service":"obligsvc","session":"`+sid+`"`) {
		t.Errorf("log missing session attribution for %q:\n%s", sid, got)
	}

	// A pretty-printed request is spec-legal JSON-RPC, and the HTTP transport
	// hands its params to the transcript verbatim: without compaction the
	// newlines smear one record across several physical lines and poison the
	// whole JSONL file for `groundwork transcript`.
	pretty := "{\n  \"jsonrpc\": \"2.0\",\n  \"id\": 9,\n  \"method\": \"tools/call\",\n  \"params\": {\n    \"name\": \"entrypoints\",\n    \"arguments\": {\n      \"service\": \"obligsvc\"\n    }\n  }\n}"
	if resp, _ = post(pretty, "s3cret", "", sid); resp.StatusCode != 200 {
		t.Fatalf("pretty-printed call status = %d", resp.StatusCode)
	}
	for i, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		if !json.Valid([]byte(line)) {
			t.Errorf("transcript line %d is not a standalone JSON object (torn record?): %q", i+1, line)
		}
	}

	if resp, _ = post(`{"jsonrpc":"2.0","id":3,"method":"ping"}`, "", "", ""); resp.StatusCode != 401 {
		t.Errorf("missing token must be 401, got %d", resp.StatusCode)
	}
	if resp, _ = post(`{"jsonrpc":"2.0","id":4,"method":"ping"}`, "wrong", "", ""); resp.StatusCode != 401 {
		t.Errorf("wrong token must be 401, got %d", resp.StatusCode)
	}
	if resp, _ = post(`{"jsonrpc":"2.0","id":5,"method":"ping"}`, "s3cret", "https://evil.example", ""); resp.StatusCode != 403 {
		t.Errorf("non-loopback Origin must be 403 even with the token, got %d", resp.StatusCode)
	}
	if resp, _ = post(`{"jsonrpc":"2.0","id":6,"method":"ping"}`, "s3cret", "http://127.0.0.1:8080", ""); resp.StatusCode != 200 {
		t.Errorf("loopback Origin must pass, got %d", resp.StatusCode)
	}
	if resp, _ = post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, "s3cret", "", ""); resp.StatusCode != 202 {
		t.Errorf("notification must be acknowledged with 202, got %d", resp.StatusCode)
	}
	if resp, _ = post(`[{"jsonrpc":"2.0","id":7,"method":"ping"}]`, "s3cret", "", ""); resp.StatusCode != 400 {
		t.Errorf("batch must be 400, got %d", resp.StatusCode)
	}

	get, err := http.NewRequest(http.MethodGet, ts.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	get.Header.Set("Authorization", "Bearer s3cret")
	gresp, err := http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	_ = gresp.Body.Close()
	if gresp.StatusCode != 405 {
		t.Errorf("GET (SSE stream request) must be 405 on this stateless server, got %d", gresp.StatusCode)
	}

	hresp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = hresp.Body.Close()
	if hresp.StatusCode != 200 {
		t.Errorf("healthz must answer without auth (liveness only), got %d", hresp.StatusCode)
	}
}

// Concurrent clients: card renders run read-locked in parallel, reload takes
// the write lock, and every transcript line is atomic and id-attributed.
// Run under -race; the assertion here is the absence of races and torn lines.
func TestServeMCPHTTPConcurrent(t *testing.T) {
	srv := &mcpServer{path: "../../testdata/groundwork/goldens/obligsvc.graph.json"}
	if err := srv.load(); err != nil {
		t.Fatal(err)
	}
	fleet := newMCPFleet(map[string]*mcpServer{"obligsvc": srv})
	var log syncBuilder
	fleet.log = &log
	ts := httptest.NewServer(fleet.httpHandler(""))
	defer ts.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			init, err := http.Post(ts.URL+"/mcp", "application/json",
				strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
			if err != nil {
				t.Error(err)
				return
			}
			sid := init.Header.Get("Mcp-Session-Id")
			_ = init.Body.Close()
			for j := 0; j < 5; j++ {
				body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"entrypoints","arguments":{}}}`
				if i == 0 && j == 2 {
					body = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"reload","arguments":{}}}`
				}
				req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
				req.Header.Set("Mcp-Session-Id", sid)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				_ = resp.Body.Close()
			}
		}(i)
	}
	wg.Wait()

	// Every line must be standalone valid JSON (atomic, never torn) and every
	// call line must carry the session id its client echoed.
	sessions := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		var e struct {
			Init    bool            `json:"init"`
			Call    json.RawMessage `json:"call"`
			Session string          `json:"session"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("torn transcript line %q: %v", line, err)
		}
		if !e.Init {
			if e.Session == "" {
				t.Errorf("call line missing session attribution: %q", line)
			}
			sessions[e.Session]++
		}
	}
	if len(sessions) != 8 {
		t.Errorf("calls attributed to %d sessions, want 8", len(sessions))
	}
	for sid, n := range sessions {
		if n != 5 {
			t.Errorf("session %s has %d calls, want 5", sid, n)
		}
	}
}

// syncBuilder is a mutex-guarded strings.Builder: the test's own log sink
// must not be the source of the race it is hunting.
type syncBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuilder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The fail-closed startup guard: exposing the server beyond loopback without
// a token is a configuration error, caught before a single request is served.
func TestGuardHTTPExposure(t *testing.T) {
	for _, tc := range []struct {
		addr, token string
		wantErr     bool
	}{
		{"127.0.0.1:8137", "", false},
		{"localhost:8137", "", false},
		{"[::1]:8137", "", false},
		{"0.0.0.0:8137", "", true},
		{":8137", "", true}, // all interfaces
		{"10.0.0.5:8137", "", true},
		{"groundwork.internal:8137", "", true},
		{"0.0.0.0:8137", "tok", false},
		{"8137", "", true}, // not host:port
	} {
		err := guardHTTPExposure(tc.addr, tc.token)
		if (err != nil) != tc.wantErr {
			t.Errorf("guardHTTPExposure(%q, token=%q) = %v, wantErr %v", tc.addr, tc.token, err, tc.wantErr)
		}
	}
}
