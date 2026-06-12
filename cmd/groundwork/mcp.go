package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/ground"
	"github.com/jyang234/golang-code-graph/internal/groundwork/impact"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
)

const mcpUsage = `usage: groundwork mcp <graph.json> [--policy <policy.json>] [--expect <stamp>] [--log <calls.jsonl>]
   or: groundwork mcp --service <name>=<graph.json> [--service <name>=<graph.json> ...] [--policy <name>=<policy.json> ...] [--expect <name>=<stamp> ...] [--log <calls.jsonl>]
add --http <addr> [--token <secret>] to either form for the team-shared streamable-HTTP transport (token also read from $GROUNDWORK_MCP_TOKEN; required off loopback)`

// cmdMCP serves the agent-facing MCP surface over stdio (IT-4): the triage,
// reach, ground, and exceptions lenses as tools an agent calls interactively
// ("now show who publishes T", "what binds the function I'm about to edit").
// Graphs are loaded once at startup and are read-only — the server holds the
// same trust posture as every other groundwork surface: it judges
// CI-generated graphs, it never generates one.
//
// Two forms. The single-graph form is unchanged. The --service form serves a
// named fleet of graphs in one session so an agent can walk a publisher in
// service A to the consumer in service B — each service keeps its own
// index/policy/stamp/staleness state, every answer stays per-service and
// honest, and the cross-service hop is explicit (the agent names the service
// it asks about). This is NOT a merged cross-service graph; `fleet-events`
// is the one fleet-wide lens, and it only joins names the contracts already
// share.
//
// Infrastructure decision, recorded here: the transport is hand-rolled
// newline-delimited JSON-RPC 2.0 (the MCP stdio framing), protocol version
// 2024-11-05, tools capability only. ~150 lines of encoding beats taking the
// engine module's first third-party server dependency for three methods.
func cmdMCP(args []string) error {
	servicePairs, args := takeValueFlags(args, "--service", "-service")
	policyPairs, args := takeValueFlags(args, "--policy", "-policy")
	expectPairs, args := takeValueFlags(args, "--expect", "-expect")
	logPath, hasLog, args := takeValueFlag(args, "--log", "-log")
	httpAddr, hasHTTP, args := takeValueFlag(args, "--http", "-http")
	token, hasToken, args := takeValueFlag(args, "--token", "-token")
	if !hasToken {
		token = os.Getenv("GROUNDWORK_MCP_TOKEN")
	}
	if hasToken && !hasHTTP {
		return fmt.Errorf("--token only applies to the --http transport\n%s", mcpUsage)
	}

	services := map[string]*mcpServer{}
	if len(servicePairs) > 0 {
		if len(args) != 0 {
			return fmt.Errorf("a positional graph and --service are mutually exclusive\n%s", mcpUsage)
		}
		for _, pair := range servicePairs {
			name, path, ok := strings.Cut(pair, "=")
			if !ok || name == "" || path == "" {
				return fmt.Errorf("--service wants <name>=<graph.json>, got %q", pair)
			}
			if !validServiceName(name) {
				return fmt.Errorf("--service name %q: names label transcripts and listings, so they are letters, digits, '.', '_', '-' only", name)
			}
			if _, dup := services[name]; dup {
				return fmt.Errorf("duplicate --service name %q", name)
			}
			services[name] = &mcpServer{path: path}
		}
		for _, pair := range expectPairs {
			name, stamp, ok := strings.Cut(pair, "=")
			srv := services[name]
			if !ok || srv == nil {
				return fmt.Errorf("--expect wants <name>=<stamp> naming a --service, got %q", pair)
			}
			srv.expect, srv.hasExpect = stamp, true
		}
	} else {
		// Single-graph form: the lone service is named by its path, so every
		// fleet-aware code path below sees one shape; calls never need to (and
		// in practice never do) pass a service argument when only one is loaded.
		if len(args) != 1 {
			return fmt.Errorf("%s", mcpUsage)
		}
		srv := &mcpServer{path: args[0]}
		if len(expectPairs) > 0 {
			srv.expect, srv.hasExpect = expectPairs[len(expectPairs)-1], true
		}
		if len(policyPairs) > 1 {
			// takeValueFlag's last-wins for a repeated flag, preserved exactly:
			// earlier values are dropped unread, never loaded-and-discarded.
			policyPairs = policyPairs[len(policyPairs)-1:]
		}
		services[args[0]] = srv
	}
	fleet := newMCPFleet(services)
	for _, name := range fleet.names {
		if err := fleet.services[name].load(); err != nil {
			return err
		}
	}
	for _, pair := range policyPairs {
		path := pair
		srv := fleet.lone()
		if len(servicePairs) > 0 {
			name, p, ok := strings.Cut(pair, "=")
			srv = fleet.services[name]
			if !ok || srv == nil {
				return fmt.Errorf("--policy wants <name>=<policy.json> naming a --service, got %q", pair)
			}
			path = p
		}
		p, err := policy.Load(path)
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
		fleet.log = f
	}
	if hasHTTP {
		return serveMCPHTTP(httpAddr, token, fleet)
	}
	return serveMCP(os.Stdin, os.Stdout, fleet)
}

// mcpFleet is the server state: one or more named services, each holding its
// own graph/policy/stamp/staleness, plus the optional call log. Tools that
// answer about one service take an optional `service` argument; with a single
// loaded service it is never needed (the lone service is the default).
//
// Concurrency (HTTP serves requests concurrently; stdio is sequential): mu
// guards the per-service state — read-held by every tool call, write-held by
// reload, the only mutator — so card renders run concurrently and never
// straddle a reload. logMu makes each transcript line (and the session
// counter) atomic; cross-client line ORDER is deliberately unguaranteed,
// which is why every line carries its session id instead of relying on
// position.
type mcpFleet struct {
	names    []string // sorted, derived from services by newMCPFleet; the single-graph form uses the graph path as the name
	services map[string]*mcpServer
	proto    string // protocol version to report; "" means 2024-11-05 (stdio)

	mu sync.RWMutex

	log      io.Writer
	logMu    sync.Mutex
	sessionN int
}

// newMCPFleet builds the fleet over its services; names are derived here,
// once, so they can never drift from the map.
func newMCPFleet(services map[string]*mcpServer) *mcpFleet {
	return &mcpFleet{names: setutil.SortedKeys(services), services: services}
}

// validServiceName restricts --service names to letters, digits, and ._-
// — they label transcript lines, fleet listings, and error texts, so the
// transcript's resolution sentinels ("*", "") and its list separators can
// never collide with a real name.
func validServiceName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// lone returns the only service when exactly one is loaded, else nil.
func (f *mcpFleet) lone() *mcpServer {
	if len(f.names) == 1 {
		return f.services[f.names[0]]
	}
	return nil
}

// resolve picks the service a call addresses. Failures are tool results the
// agent can read and correct: an unknown name lists the loaded ones, and an
// omitted name with several services loaded asks for the hop to be explicit.
func (f *mcpFleet) resolve(name string) (*mcpServer, map[string]any) {
	if name == "" {
		if srv := f.lone(); srv != nil {
			return srv, nil
		}
		return nil, toolError(fmt.Sprintf("%d services are loaded; pass service: one of %s", len(f.names), strings.Join(f.names, ", ")))
	}
	if srv, ok := f.services[name]; ok {
		return srv, nil
	}
	return nil, toolError(fmt.Sprintf("unknown service %q; loaded: %s", name, strings.Join(f.names, ", ")))
}

// staleNotes flags every service whose graph file changed on disk — the
// fleet-wide listings disclose staleness per service, by name, just as the
// per-service answers do for their own graph.
func (f *mcpFleet) staleNotes() string {
	var b strings.Builder
	for _, name := range f.names {
		if f.services[name].isStale() {
			fmt.Fprintf(&b, "⚠️ service %s: the graph file changed on disk after this server loaded it — call the reload tool before trusting answers about it\n", name)
		}
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	return b.String()
}

// mcpServer is the per-service state: the loaded graph, its file identity
// (for staleness detection), and the optional policy.
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

// isStale reports whether the graph file changed on disk after load.
func (s *mcpServer) isStale() bool {
	st, err := os.Stat(s.path)
	return err == nil && !st.ModTime().Equal(s.mtime)
}

// staleNote flags a changed graph file on every response rather than silently
// reloading: answers must never change mid-session without disclosure.
func (s *mcpServer) staleNote() string {
	if s.isStale() {
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

// serveMCP runs the stdio request loop until EOF. Notifications (no id) are
// consumed silently per JSON-RPC; tool failures are MCP tool results with
// isError, not protocol errors, so the agent can read and recover from them.
// Session identity is transport-scoped, so the transport mints it: stdio has
// one client, whose current session is whatever initialize last began.
func serveMCP(r io.Reader, w io.Writer, fleet *mcpFleet) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	enc := json.NewEncoder(w)
	session := ""
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil || req.ID == nil {
			continue // malformed or a notification: nothing to answer
		}
		if req.Method == "initialize" {
			session = fleet.newSession()
		}
		if err := enc.Encode(fleet.dispatch(req, session)); err != nil {
			return err
		}
	}
	return sc.Err()
}

// newSession issues the next session id and records the boundary in the
// transcript. Ids are sequential, not random — deterministic, like the rest
// of the transcript — and they are attribution labels only: the fleet keeps
// no per-session state. A session that begins and then asks nothing is
// itself E4 evidence, which is why the boundary is recorded immediately.
func (f *mcpFleet) newSession() string {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	f.sessionN++
	sid := strconv.Itoa(f.sessionN)
	if f.log != nil {
		_, _ = fmt.Fprintf(f.log, "{\"init\":true,\"session\":%q}\n", sid)
	}
	return sid
}

// logCall writes one transcript line, AFTER the call so the line carries its
// resolution: the service that answered ("*" for the fleet-wide lenses,
// absent when resolution failed), the session the call belongs to, and the
// isError outcome. Deterministic on purpose — no timestamps — so a replayed
// drill produces identical bytes; `groundwork transcript` is the reader.
// Only the line itself is atomic: concurrent HTTP clients interleave lines
// freely, and attribution survives because it rides the session id, never
// the line order.
func (f *mcpFleet) logCall(params json.RawMessage, service, session string, result map[string]any) {
	if f.log == nil {
		return
	}
	if len(params) == 0 {
		params = json.RawMessage("null")
	}
	// Compact before splicing: stdio input is line-delimited, but the HTTP
	// transport hands over the client's params verbatim — a pretty-printed
	// (spec-legal) request would smear this record across multiple physical
	// lines and poison the whole JSONL file for the transcript reader.
	var compact bytes.Buffer
	if err := json.Compact(&compact, params); err != nil {
		return // not valid JSON; dispatch already rejected the request
	}
	line := append([]byte(`{"call":`), compact.Bytes()...)
	if service != "" {
		q, _ := json.Marshal(service)
		line = append(append(line, []byte(`,"service":`)...), q...)
	}
	if session != "" {
		q, _ := json.Marshal(session)
		line = append(append(line, []byte(`,"session":`)...), q...)
	}
	if isErr, _ := result["isError"].(bool); isErr {
		line = append(line, []byte(`,"isError":true`)...)
	}
	f.logMu.Lock()
	_, _ = f.log.Write(append(line, '}', '\n'))
	f.logMu.Unlock()
}

// dispatch answers one JSON-RPC request — the transport-independent core
// shared by the stdio loop and the streamable-HTTP handler. session is the
// transport's attribution label for the transcript ("" when the client never
// initialized or did not echo its id).
func (f *mcpFleet) dispatch(req rpcRequest, session string) rpcResponse {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		proto := f.proto
		if proto == "" {
			proto = "2024-11-05"
		}
		resp.Result = map[string]any{
			"protocolVersion": proto,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "groundwork", "version": version},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": toolDefs()}
	case "tools/call":
		result, service := f.callTool(req.Params)
		f.logCall(req.Params, service, session, result)
		resp.Result = result
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func toolDefs() []map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	obj := func(props map[string]any, required ...string) map[string]any {
		props["service"] = str("service name, when more than one graph is loaded (default: the only loaded service)")
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
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
			"description": "List a service's named roots (HTTP routes, consumed topics) with their handler functions — what triage's route/event symptoms can address. With several services loaded and no service argument, lists across the whole fleet, prefixed by service.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "fleet-events",
			"description": "Cross-service event lens: every bus event a loaded service publishes or consumes, joined by name across the fleet — who publishes what, who consumes it. Covers loaded services only and discloses dynamically-named publishes it cannot see.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "fitness",
			"description": "Evaluate every policy invariant against a loaded graph: violations, cautions, and obligation verdicts. Requires the service to be started with --policy.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "reload",
			"description": "Reload a service's graph from disk after a redeploy changed it (the server flags staleness on every response; it never reloads silently). Optionally re-verify identity with expect.",
			"inputSchema": obj(map[string]any{"expect": str("stamp the reloaded graph must carry (e.g. the new deployed SHA)")}),
		},
		{
			"name":        "exceptions",
			"description": "Audit every policy allow-list entry against a loaded graph; DEAD entries suppress nothing and should be deleted. Requires the service to be started with --policy.",
			"inputSchema": obj(map[string]any{}),
		},
	}
}

// toolArgs is the union of every tool's arguments.
type toolArgs struct {
	Service string `json:"service"`
	FQN     string `json:"fqn"`
	Frame   string `json:"frame"`
	Route   string `json:"route"`
	Table   string `json:"table"`
	Event   string `json:"event"`
	Peer    string `json:"peer"`
	Fail    bool   `json:"fail"`
	Expect  string `json:"expect"`
}

// callTool dispatches one tools/call. Failures are tool results (isError),
// never protocol errors: the agent reads the reason and corrects its call.
// The fleet-wide lenses (fleet-events, the no-service entrypoints listing)
// are answered here; everything else resolves to one service first. The
// second return is the transcript's resolution label: the answering
// service's name, "*" for a fleet-wide answer, "" when resolution failed.
//
// Every tool reads per-service state and runs read-locked, concurrently;
// reload, the lone mutator, takes the write lock so no card can straddle it.
func (f *mcpFleet) callTool(params json.RawMessage) (map[string]any, string) {
	var call struct {
		Name      string   `json:"name"`
		Arguments toolArgs `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return toolError("malformed tools/call params: " + err.Error()), ""
	}
	if call.Name == "reload" {
		f.mu.Lock()
		defer f.mu.Unlock()
	} else {
		f.mu.RLock()
		defer f.mu.RUnlock()
	}
	if call.Name == "fleet-events" {
		return f.fleetEvents(), "*"
	}
	if call.Name == "entrypoints" && call.Arguments.Service == "" && len(f.names) > 1 {
		return f.fleetEntrypoints(), "*"
	}
	srv, errRes := f.resolve(call.Arguments.Service)
	if errRes != nil {
		return errRes, ""
	}
	service := call.Arguments.Service
	if service == "" {
		service = f.names[0] // the lone service: resolve already enforced it
	}
	return srv.call(call.Name, call.Arguments), service
}

// fleetEntrypoints lists every service's named roots, prefixed by service —
// the fleet directory an agent orients with before making an explicit hop.
// Stale notes and the empty-case disclosure are independent: a stale warning
// must never swallow the "there are no entrypoints" answer.
func (f *mcpFleet) fleetEntrypoints() map[string]any {
	stale := f.staleNotes()
	var b strings.Builder
	for _, name := range f.names {
		for _, ep := range f.services[name].ix.Entrypoints() {
			fmt.Fprintf(&b, "%-12s %-9s %-40s → %s\n", name, ep.Kind, ep.Name, ep.Fn)
		}
	}
	if b.Len() == 0 {
		return toolText(stale + "no named entrypoints in any loaded graph (routes behind uncovered routers are absent — see the docs)")
	}
	return toolText(stale + b.String())
}

// fleetEvents joins the loaded graphs' bus surfaces by event name: which
// service publishes each event and which consumes it. The join vocabulary is
// the boundary contracts' — published/consumed names match across services —
// so no merged graph is needed or implied; an empty side is reported as
// outside the loaded fleet, never guessed. The surface itself comes from
// graph.Index.BusEffects (the schema owner decodes its own labels), plus
// consumer entrypoints. Dynamically-named bus effects are disclosed per
// service: events this lens cannot name are absent from it.
func (f *mcpFleet) fleetEvents() map[string]any {
	pub, con := map[string]map[string]bool{}, map[string]map[string]bool{}
	dynamic := map[string]int{}
	add := func(m map[string]map[string]bool, event, service string) {
		if m[event] == nil {
			m[event] = map[string]bool{}
		}
		m[event][service] = true
	}
	for _, name := range f.names {
		ix := f.services[name].ix
		effects, dyn := ix.BusEffects()
		dynamic[name] = dyn
		for _, be := range effects {
			switch be.Op {
			case graph.BusPublish:
				add(pub, be.Event, name)
			case graph.BusConsume:
				add(con, be.Event, name)
			}
		}
		for _, ep := range ix.Entrypoints() {
			if ep.Kind == "consumer" {
				add(con, ep.Name, name)
			}
		}
	}
	events := map[string]bool{}
	for ev := range pub {
		events[ev] = true
	}
	for ev := range con {
		events[ev] = true
	}
	if len(events) == 0 {
		return toolText("no bus events in any loaded graph")
	}
	side := func(m map[string]bool) string {
		if len(m) == 0 {
			return "(none in the loaded fleet)"
		}
		return strings.Join(setutil.SortedKeys(m), ", ")
	}
	var b strings.Builder
	b.WriteString(f.staleNotes())
	for _, ev := range setutil.SortedKeys(events) {
		fmt.Fprintf(&b, "%-28s published by: %-28s consumed by: %s\n", ev, side(pub[ev]), side(con[ev]))
	}
	for _, name := range f.names {
		if n := dynamic[name]; n > 0 {
			fmt.Fprintf(&b, "\n⚠️ %s has %d dynamically-named bus effect(s) — events it cannot name are absent from this lens", name, n)
		}
	}
	return toolText(b.String())
}

// call answers one per-service tool against this service's graph and policy.
func (s *mcpServer) call(name string, a toolArgs) map[string]any {
	ix, p := s.ix, s.p
	stale := s.staleNote()
	withStale := func(r map[string]any) map[string]any {
		if stale == "" {
			return r
		}
		if content, ok := r["content"].([]map[string]any); ok && len(content) > 0 {
			content[0]["text"] = stale + content[0]["text"].(string)
		}
		return r
	}
	switch name {
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
		if a.Frame != "" {
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
			return withStale(toolText("no allow-list entries configured"))
		}
		var b strings.Builder
		for _, x := range xs {
			fmt.Fprintln(&b, x)
		}
		fmt.Fprintf(&b, "\n%d dead exception(s)\n", fitness.DeadCount(xs))
		return withStale(toolText(b.String()))
	default:
		return toolError("unknown tool: " + name)
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
