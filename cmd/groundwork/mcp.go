package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/ground"
	"github.com/jyang234/golang-code-graph/internal/groundwork/impact"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// cmdMCP serves the agent-facing MCP surface over stdio (IT-4): the triage,
// reach, ground, and exceptions lenses as tools an agent calls interactively
// ("now show who publishes T", "what binds the function I'm about to edit").
// The graph is loaded once at startup and is read-only — the server holds the
// same trust posture as every other groundwork surface: it judges a
// CI-generated graph, it never generates one.
//
// Infrastructure decision, recorded here: the transport is hand-rolled
// newline-delimited JSON-RPC 2.0 (the MCP stdio framing), protocol version
// 2024-11-05, tools capability only. ~150 lines of encoding beats taking the
// engine module's first third-party server dependency for three methods.
func cmdMCP(args []string) error {
	policyPath, hasPolicy, args := takeValueFlag(args, "--policy", "-policy")
	expect, hasExpect, args := takeValueFlag(args, "--expect", "-expect")
	logPath, hasLog, args := takeValueFlag(args, "--log", "-log")
	if len(args) != 1 {
		return fmt.Errorf("usage: groundwork mcp <graph.json> [--policy <policy.json>] [--expect <stamp>] [--log <calls.jsonl>]")
	}
	srv := &mcpServer{path: args[0], expect: expect, hasExpect: hasExpect}
	if err := srv.load(); err != nil {
		return err
	}
	if hasPolicy {
		p, err := policy.Load(policyPath)
		if err != nil {
			return err
		}
		srv.p = p
	}
	if hasLog {
		// The E4 measurement apparatus: a deterministic transcript of tool
		// calls, one JSON line each, for analyzing how an agent actually used
		// the surface during a drill.
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		srv.log = f
	}
	return serveMCP(os.Stdin, os.Stdout, srv)
}

// mcpServer is the per-session state: the loaded graph, its file identity
// (for staleness detection), the optional policy, and the optional call log.
// NO WRITE TOOLS, EVER: a tool that edited policy or rules would let the
// agent author its own guardrails — the one thing the trust model forbids.
// Graph generation likewise stays in CLI/CI; this server only ever reads.
type mcpServer struct {
	path      string
	mtime     time.Time
	ix        *graph.Index
	p         *policy.Policy
	expect    string
	hasExpect bool
	log       io.Writer
}

func (s *mcpServer) load() error {
	g, err := graph.LoadFile(s.path)
	if err != nil {
		return err
	}
	if err := verifyStamp(g, s.expect, s.hasExpect); err != nil {
		return err
	}
	if st, err := os.Stat(s.path); err == nil {
		s.mtime = st.ModTime()
	}
	s.ix = graph.NewIndex(g)
	return nil
}

// staleNote flags a changed graph file on every response rather than silently
// reloading: answers must never change mid-session without disclosure.
func (s *mcpServer) staleNote() string {
	st, err := os.Stat(s.path)
	if err == nil && !st.ModTime().Equal(s.mtime) {
		return "⚠️ the graph file changed on disk after this server loaded it — call the reload tool (or restart) before trusting further answers\n\n"
	}
	return ""
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// serveMCP runs the request loop until EOF. Notifications (no id) are
// consumed silently per JSON-RPC; tool failures are MCP tool results with
// isError, not protocol errors, so the agent can read and recover from them.
func serveMCP(r io.Reader, w io.Writer, srv *mcpServer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil || req.ID == nil {
			continue // malformed or a notification: nothing to answer
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			resp.Result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "groundwork", "version": version},
			}
		case "ping":
			resp.Result = map[string]any{}
		case "tools/list":
			resp.Result = map[string]any{"tools": toolDefs()}
		case "tools/call":
			if srv.log != nil {
				_, _ = srv.log.Write(append(append([]byte(`{"call":`), req.Params...), '}', 10))
			}
			resp.Result = srv.callTool(req.Params)
		default:
			resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func toolDefs() []map[string]any {
	obj := func(props map[string]any, required ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return []map[string]any{
		{
			"name":        "ground",
			"description": "Pre-edit grounding card for one function: identity, neighborhood, reachable effects, the rules that bind any edit there, and the blind spots touching those claims. Call BEFORE editing.",
			"inputSchema": obj(map[string]any{"fqn": str("fully-qualified function name from the graph")}, "fqn"),
		},
		{
			"name":        "reach",
			"description": "Bidirectional blast radius of one function: implicated entrypoints, upstream callers, reachable boundary effects, blind spots.",
			"inputSchema": obj(map[string]any{"fqn": str("fully-qualified function name from the graph")}, "fqn"),
		},
		{
			"name":        "triage",
			"description": "Incident triage card from a symptom. Provide exactly one of frame/route/table/event/peer; set fail=true for the what-if fault framing (includes effects possibly committed before the fault).",
			"inputSchema": obj(map[string]any{
				"frame": str("stack frame: FQN, runtime frame form, or token-bounded suffix"),
				"route": str("HTTP route, e.g. 'POST /api/v1/loans/{id}' — segment-matched, method optional"),
				"table": str("DB table name"),
				"event": str("bus event name"),
				"peer":  str("outbound peer name"),
				"fail":  map[string]any{"type": "boolean", "description": "treat the resolved suspects as failing"},
			}),
		},
		{
			"name":        "entrypoints",
			"description": "List the service's named roots (HTTP routes, consumed topics) with their handler functions — what triage's route/event symptoms can address.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "fitness",
			"description": "Evaluate every policy invariant against the loaded graph: violations, cautions, and obligation verdicts. Requires the server to be started with --policy.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "reload",
			"description": "Reload the graph from disk after a redeploy changed it (the server flags staleness on every response; it never reloads silently). Optionally re-verify identity with expect.",
			"inputSchema": obj(map[string]any{"expect": str("stamp the reloaded graph must carry (e.g. the new deployed SHA)")}),
		},
		{
			"name":        "exceptions",
			"description": "Audit every policy allow-list entry against the graph; DEAD entries suppress nothing and should be deleted. Requires the server to be started with --policy.",
			"inputSchema": obj(map[string]any{}),
		},
	}
}

// callTool dispatches one tools/call. Failures are tool results (isError),
// never protocol errors: the agent reads the reason and corrects its call.
func (s *mcpServer) callTool(params json.RawMessage) map[string]any {
	ix, p := s.ix, s.p
	stale := s.staleNote()
	var call struct {
		Name      string `json:"name"`
		Arguments struct {
			FQN    string `json:"fqn"`
			Frame  string `json:"frame"`
			Route  string `json:"route"`
			Table  string `json:"table"`
			Event  string `json:"event"`
			Peer   string `json:"peer"`
			Fail   bool   `json:"fail"`
			Expect string `json:"expect"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return toolError("malformed tools/call params: " + err.Error())
	}
	a := call.Arguments
	withStale := func(r map[string]any) map[string]any {
		if stale == "" {
			return r
		}
		if content, ok := r["content"].([]map[string]any); ok && len(content) > 0 {
			content[0]["text"] = stale + content[0]["text"].(string)
		}
		return r
	}
	_ = withStale
	switch call.Name {
	case "entrypoints":
		eps := ix.Entrypoints()
		if len(eps) == 0 {
			return withStale(toolText("no named entrypoints in this graph (routes behind uncovered routers are absent — see the docs)"))
		}
		var b strings.Builder
		for _, ep := range eps {
			fmt.Fprintf(&b, "%-9s %-40s → %s\n", ep.Kind, ep.Name, ep.Fn)
		}
		return withStale(toolText(b.String()))
	case "fitness":
		if p == nil {
			return toolError("the server was started without --policy; fitness needs one")
		}
		res := fitness.Check(p, ix)
		var b strings.Builder
		for _, f := range res.Violations() {
			fmt.Fprintf(&b, "⛔ [%s] %s\n", f.Rule, f.Summary)
		}
		for _, f := range res.Cautions() {
			fmt.Fprintf(&b, "⚠️ [%s] %s\n", f.Rule, f.Summary)
		}
		if len(res.Findings) == 0 {
			b.WriteString("all invariants hold; no cautions\n")
		}
		return withStale(toolText(b.String()))
	case "reload":
		old := s.expect
		oldHas := s.hasExpect
		if a.Expect != "" {
			s.expect, s.hasExpect = a.Expect, true
		}
		if err := s.load(); err != nil {
			s.expect, s.hasExpect = old, oldHas
			return toolError("reload failed (previous graph still served): " + err.Error())
		}
		return toolText("graph reloaded from " + s.path)
	case "ground":
		card, err := ground.For(ix, p, a.FQN)
		if err != nil {
			return toolError(err.Error())
		}
		return withStale(toolText(card.Render()))
	case "reach":
		if !ix.Has(a.FQN) {
			return toolError(fmt.Sprintf("no function %q in graph", a.FQN))
		}
		return withStale(toolText(impact.ForNodes(ix, []string{a.FQN}).Render()))
	case "triage":
		var res impact.Resolution
		set := 0
		switch {
		case a.Frame != "":
			res, set = impact.ResolveFrame(ix, a.Frame), set+1
		}
		if a.Route != "" {
			res, set = impact.ResolveRoute(ix, a.Route), set+1
		}
		if a.Table != "" {
			res, set = impact.ResolveTable(ix, a.Table), set+1
		}
		if a.Event != "" {
			res, set = impact.ResolveEvent(ix, a.Event), set+1
		}
		if a.Peer != "" {
			res, set = impact.ResolvePeer(ix, a.Peer), set+1
		}
		if set != 1 {
			return toolError(fmt.Sprintf("exactly one of frame/route/table/event/peer is required (got %d)", set))
		}
		if len(res.Matches) == 0 && len(res.Possible) == 0 {
			return toolError("symptom resolved to nothing in this graph")
		}
		suspects := append(append([]string{}, res.Matches...), res.Possible...)
		card := impact.ForNodes(ix, suspects)
		if a.Fail {
			card = impact.ForFault(ix, suspects)
		}
		var b strings.Builder
		if res.Ambiguous {
			fmt.Fprintf(&b, "symptom is ambiguous — %d candidates, all included\n\n", len(res.Matches))
		}
		if len(res.Possible) > 0 {
			fmt.Fprintf(&b, "%d possible match(es) via <dynamic> boundary effects, included and flagged\n\n", len(res.Possible))
		}
		b.WriteString(card.Render())
		return withStale(toolText(b.String()))
	case "exceptions":
		if p == nil {
			return toolError("the server was started without --policy; exceptions needs one")
		}
		xs := fitness.Exceptions(p, ix)
		if len(xs) == 0 {
			return toolText("no allow-list entries configured")
		}
		var b strings.Builder
		for _, x := range xs {
			fmt.Fprintln(&b, x)
		}
		fmt.Fprintf(&b, "\n%d dead exception(s)\n", fitness.DeadCount(xs))
		return toolText(b.String())
	default:
		return toolError("unknown tool: " + call.Name)
	}
}

func toolText(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

func toolError(msg string) map[string]any {
	r := toolText(msg)
	r["isError"] = true
	return r
}
