package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

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
	if len(args) != 1 {
		return fmt.Errorf("usage: groundwork mcp <graph.json> [--policy <policy.json>] [--expect <stamp>]")
	}
	g, err := graph.LoadFile(args[0])
	if err != nil {
		return err
	}
	if err := verifyStamp(g, expect, hasExpect); err != nil {
		return err
	}
	var p *policy.Policy
	if hasPolicy {
		if p, err = policy.Load(policyPath); err != nil {
			return err
		}
	}
	return serveMCP(os.Stdin, os.Stdout, graph.NewIndex(g), p)
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
func serveMCP(r io.Reader, w io.Writer, ix *graph.Index, p *policy.Policy) error {
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
			resp.Result = callTool(req.Params, ix, p)
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
			"description": "Incident triage card from a symptom. Provide exactly one of frame/table/event/peer; set fail=true for the what-if fault framing (includes effects possibly committed before the fault).",
			"inputSchema": obj(map[string]any{
				"frame": str("stack frame: FQN, runtime frame form, or token-bounded suffix"),
				"table": str("DB table name"),
				"event": str("bus event name"),
				"peer":  str("outbound peer name"),
				"fail":  map[string]any{"type": "boolean", "description": "treat the resolved suspects as failing"},
			}),
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
func callTool(params json.RawMessage, ix *graph.Index, p *policy.Policy) map[string]any {
	var call struct {
		Name      string `json:"name"`
		Arguments struct {
			FQN   string `json:"fqn"`
			Frame string `json:"frame"`
			Table string `json:"table"`
			Event string `json:"event"`
			Peer  string `json:"peer"`
			Fail  bool   `json:"fail"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return toolError("malformed tools/call params: " + err.Error())
	}
	a := call.Arguments
	switch call.Name {
	case "ground":
		card, err := ground.For(ix, p, a.FQN)
		if err != nil {
			return toolError(err.Error())
		}
		return toolText(card.Render())
	case "reach":
		if !ix.Has(a.FQN) {
			return toolError(fmt.Sprintf("no function %q in graph", a.FQN))
		}
		return toolText(impact.ForNodes(ix, []string{a.FQN}).Render())
	case "triage":
		var res impact.Resolution
		set := 0
		switch {
		case a.Frame != "":
			res, set = impact.ResolveFrame(ix, a.Frame), set+1
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
			return toolError(fmt.Sprintf("exactly one of frame/table/event/peer is required (got %d)", set))
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
		return toolText(b.String())
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
