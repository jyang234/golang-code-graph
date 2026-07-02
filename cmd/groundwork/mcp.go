package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/groundwork/chains"
	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/ground"
	"github.com/jyang234/golang-code-graph/internal/groundwork/impact"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
	"github.com/jyang234/golang-code-graph/internal/impeach"
	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
	"github.com/jyang234/golang-code-graph/ir"

	"github.com/jyang234/golang-code-graph/capture"

	"gopkg.in/yaml.v3"
)

const mcpUsage = `usage: groundwork mcp <graph.json> [--policy <policy.json>] [--corpus <dir>] [--capture production|integration] [--expect <stamp>] [--log <calls.jsonl>]
   or: groundwork mcp --service <name>=<graph.json> [--service <name>=<graph.json> ...] [--policy <name>=<policy.json> ...] [--corpus <name>=<dir> ...] [--capture <name>=<grade> ...] [--expect <name>=<stamp> ...] [--log <calls.jsonl>]
--corpus enables the audit-only impeach lens (a committed *.golden.json behavioral corpus); --capture asserts its fidelity grade and requires --corpus.
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
	corpusPairs, args := takeValueFlags(args, "--corpus", "-corpus")
	capturePairs, args := takeValueFlags(args, "--capture", "-capture")
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
		// takeValueFlag's last-wins for a repeated flag, preserved exactly: earlier
		// values are dropped unread, never loaded-and-discarded.
		if len(policyPairs) > 1 {
			policyPairs = policyPairs[len(policyPairs)-1:]
		}
		if len(corpusPairs) > 1 {
			corpusPairs = corpusPairs[len(corpusPairs)-1:]
		}
		if len(capturePairs) > 1 {
			capturePairs = capturePairs[len(capturePairs)-1:]
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
	// --capture asserts a behavioral corpus's fidelity grade. Only production and
	// integration may be asserted (capture.AssertableGrade — the ONE source the verify
	// CLI validates against too); an unrecognized or empty grade is REFUSED here, not
	// laundered into a silent CAPTURE-UNTRUSTED downgrade deep in the ladder (tenet 2).
	for _, pair := range capturePairs {
		grade := pair
		if len(servicePairs) > 0 {
			name, g, ok := strings.Cut(pair, "=")
			srv := fleet.services[name]
			if !ok || srv == nil {
				return fmt.Errorf("--capture wants <name>=<grade> naming a --service, got %q", pair)
			}
			grade = g
			if !capture.AssertableGrade(grade) {
				return fmt.Errorf("--capture for service %q: grade must be %q or %q, got %q", name, capture.CaptureProduction, capture.CaptureIntegration, grade)
			}
			srv.capture = grade
			continue
		}
		if !capture.AssertableGrade(grade) {
			return fmt.Errorf("--capture: grade must be %q or %q, got %q", capture.CaptureProduction, capture.CaptureIntegration, grade)
		}
		fleet.lone().capture = grade
	}
	for _, pair := range corpusPairs {
		dir := pair
		srv := fleet.lone()
		if len(servicePairs) > 0 {
			name, d, ok := strings.Cut(pair, "=")
			srv = fleet.services[name]
			if !ok || srv == nil {
				return fmt.Errorf("--corpus wants <name>=<dir> naming a --service, got %q", pair)
			}
			dir = d
		}
		// One source of truth: the same recursive, fail-closed loader the verify gate
		// uses (loadCommittedCorpus), so the MCP audit can never see a different trace
		// set than the gate would from the same directory.
		traces, err := loadCommittedCorpus(dir)
		if err != nil {
			return err
		}
		srv.corpus = traces
		srv.corpusDir = dir
	}
	// Fail closed on a dangling impeach configuration, symmetrically: a capture grade
	// with no corpus to grade, OR a corpus with no policy to integrate its witnesses
	// against (impeach needs both — Resolve folds witnesses into must_not_reach). Name
	// the service only in the fleet form, where the user supplied the name; the
	// single-graph form has a synthetic name (the graph path) that would only confuse.
	// len(), not == nil: loadCommittedCorpus never returns a non-nil empty slice today,
	// but the guard states the intent ("no corpus") rather than relying on that.
	for _, name := range fleet.names {
		s := fleet.services[name]
		svcCtx := ""
		if len(servicePairs) > 0 {
			svcCtx = fmt.Sprintf(" for service %q", name)
		}
		if s.capture != "" && len(s.corpus) == 0 {
			return fmt.Errorf("--capture%s requires --corpus (it asserts the fidelity grade of a behavioral corpus)", svcCtx)
		}
		if len(s.corpus) > 0 && s.p == nil {
			return fmt.Errorf("--corpus%s requires --policy (the impeach audit integrates witnesses against the policy's must_not_reach rules)", svcCtx)
		}
		// Inputs are final: render the impeach audit once (refreshed only on reload).
		s.computeImpeach()
	}
	if hasLog {
		// The E4 measurement apparatus: a deterministic transcript of tool
		// calls, one JSON line each, for analyzing how an agent actually used
		// the surface during a drill.
		// The log is O_APPEND, so a server restart writes on top of a prior run's
		// transcript. Per-process session ids restarting at 1 would then collide with
		// the earlier run's "1","2",… and silently MERGE unrelated sessions in the E4
		// evidence (M-34). Continue numbering past the highest session already in the
		// file: deterministic (a pure function of the existing content — a replayed
		// drill over an empty/identical log still starts at 1), non-destructive, and
		// collision-free across restarts.
		if data, rerr := os.ReadFile(logPath); rerr == nil {
			fleet.sessionN = maxLoggedSession(data)
		}
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

	// corpus is the committed behavioral corpus the impeach lens audits the graph
	// against — loaded ONCE at startup, exactly like p (policy): a load-once input,
	// not staleness-tracked (only the graph is, via reload — it alone changes under a
	// running server by design, on redeploy; a committed corpus changes only on a git
	// action in the checkout, where a restart is the natural refresh). The impeach
	// card discloses this load-once contract (corpusDir + count) so the freshness
	// boundary is legible rather than silent. capture is the optional human-asserted
	// capture-fidelity grade reconciled against the corpus's own self-declared grade
	// (§12.6); empty means "take the corpus's grade verbatim".
	corpus    []*ir.CanonicalTrace
	corpusDir string
	capture   string

	// impeachBody is the rendered impeach audit, computed ONCE when corpus+policy are
	// present (computeImpeach) and refreshed only on reload — the audit is a pure
	// function of (ix, corpus, capture, rules), all immutable between reloads, so
	// recomputing the graph-reachability + corpus-hash work on every call would be
	// wasted (it ran under the read lock, so concurrent callers repeated it). "" when
	// the lens is unconfigured; the per-call staleNote prefix stays dynamic.
	impeachBody string
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
		if err := json.Unmarshal(line, &req); err != nil {
			// Malformed JSON: a client that sent a request (not a notification)
			// would hang forever waiting on a response. Answer with a JSON-RPC parse
			// error and a null id (the id is unrecoverable from unparseable input),
			// matching the HTTP transport's 400 rather than silently dropping it.
			if encErr := enc.Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}}); encErr != nil {
				return encErr
			}
			continue
		}
		if req.ID == nil {
			continue // a notification: JSON-RPC produces no response
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

// maxLoggedSession returns the highest session number already recorded in an
// existing transcript, or 0 for an empty/new log. It parses only the plain-integer
// session ids this server emits; a non-numeric id (none are written today) is
// ignored. Used to continue numbering across a restart so session ids never collide
// on an appended log (M-34).
func maxLoggedSession(data []byte) int {
	max := 0
	for _, m := range sessionIDRe.FindAllSubmatch(data, -1) {
		if n, err := strconv.Atoi(string(m[1])); err == nil && n > max {
			max = n
		}
	}
	return max
}

var sessionIDRe = regexp.MustCompile(`"session":"(\d+)"`)

// newSession issues the next session id and records the boundary in the
// transcript. Ids are sequential, not random — deterministic, like the rest
// of the transcript — and they are attribution labels only: the fleet keeps
// no per-session state. A session that begins and then asks nothing is
// itself E4 evidence, which is why the boundary is recorded immediately.
//
// Numbering continues past any session already present in an appended log
// (maxLoggedSession, set at startup), so a restart cannot reissue an id the prior
// run used and merge two runs' sessions in the transcript.
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
			"name":        "annotate",
			"description": "Propose human/AI CONTEXT for a blind spot — what lies BEHIND a seam the analysis cannot see past (the I/O behind an ExternalBoundaryCall, the work inside a goroutine). Validates (site, kind) against this graph's live blind-spot manifest and returns ready-to-commit .flowmap.yaml under static.annotations. READ-ONLY: it writes no file — you persist the snippet — and an annotation is disclosure-only, so it never changes a count or a verdict. Errors (orphan site, ambiguous kind) name the kinds actually present so you can correct without re-deriving.",
			"inputSchema": obj(map[string]any{
				"site":  str("FQN of the blind spot to annotate (as it appears in a ground/reach card)"),
				"kind":  str("blind-spot kind, e.g. ExternalBoundaryCall; omit only when the site has exactly one"),
				"note":  str("the context: what happens beyond this seam"),
				"by":    str("authorship for audit — a human handle or an agent id/model"),
				"claim": str("optional canonical effect key the seam hides (e.g. 'PUBLISH email.sent', 'db DELETE ledger'); the impeach lens grades it CONFIRMED/UNCONFIRMED against the corpus"),
			}, "site", "note"),
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
			"name":        "chains",
			"description": "Cross-service effect chains (CX-5): per bus event, a happens-before chain whose links are labeled proven (a per-service graph fact: the publish's commit ordering, the consumer handler's effects/obligations) or assumed (the declared broker guarantee, printed verbatim, never inferred). Answers 'if this publish faulted, what already committed, and what does the consumer do with the event?'. Observational, never a gate; the broker block is read from a service's --policy and flagged UNSIGNED until a human warrants it.",
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
			"description": "Audit every policy allow-list entry and rule pattern against a loaded graph; DEAD entries suppress or bind nothing and should be fixed or deleted. Requires the service to be started with --policy.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "impeach",
			"description": "AUDIT-ONLY, never a gate: join the loaded graph against its committed behavioral corpus (--corpus) and disclose impeachment candidates — effects OBSERVED in the corpus where static analysis placed none. Each is classified through the downgrade ladder (IMPEACHMENT, or a specific downgrade like VERSION-SKEW / CAPTURE-UNTRUSTED), with the localized site where static lost the effect. Also grades blind-spot annotations against the corpus: WITNESSED when an observed effect is severed at the annotation's site (the corpus corroborates the SEAM, not the note's prose), UNWITNESSED otherwise. This is disclosure for an agent before an edit; the deterministic MERGE gate is `groundwork verify --corpus` over CI-built base/branch graphs, never this lens (the loaded graph may be a local build). Requires the service to be started with --corpus and --policy.",
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
	Site    string `json:"site"`
	Kind    string `json:"kind"`
	Note    string `json:"note"`
	By      string `json:"by"`
	Claim   string `json:"claim"`
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
	if call.Name == "chains" {
		return f.chains(), "*"
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
		// "No bus events" must not read as "this fleet does no messaging" when
		// every bus effect was dynamically named: the lens is INERT here, not
		// empty. Disclose the dynamic effects it could not name so the silence is
		// legible — the join needs statically-nameable topics, which <dynamic>
		// publish/consume does not provide (F5).
		var b strings.Builder
		b.WriteString("no statically-named bus events in any loaded graph")
		total := 0
		for _, name := range f.names {
			if n := dynamic[name]; n > 0 {
				fmt.Fprintf(&b, "\n⚠️ %s has %d dynamically-named bus effect(s) — this lens needs statically-nameable topics and is inert on <dynamic> messaging here", name, n)
				total += n
			}
		}
		if total > 0 {
			b.WriteString("\n(the behavioral pipeline resolves these runtime topic names; static analysis cannot)")
		}
		return toolText(b.String())
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

// chains composes the cross-service effect-chain cards over the loaded fleet
// (CX-5). The broker guarantee is read from the loaded services' policies: the
// bus is one thing, so a guarantee declared by two services that disagree is
// refused rather than printed as if authored. staleNotes prefixes any
// since-redeployed graph, the same disclosure every fleet lens carries.
func (f *mcpFleet) chains() map[string]any {
	var fleet []chains.Service
	var perService []map[string]policy.Broker
	for _, name := range f.names {
		s := f.services[name]
		fleet = append(fleet, chains.Service{Name: name, Index: s.ix})
		if s.p == nil {
			continue
		}
		perService = append(perService, s.p.Brokers)
	}
	// policy.MergeBrokers is the ONE merge shared with the CLI `chains` command; it
	// returns the conflicting names SORTED so the error is byte-identical run to run
	// (ranging s.p.Brokers directly otherwise reported whichever conflict the map
	// iteration visited), and both surfaces refuse on exactly the same condition (M-4).
	brokers, conflicts := policy.MergeBrokers(perService)
	if len(conflicts) > 0 {
		return toolError(fmt.Sprintf("broker(s) %s declared differently by more than one loaded policy; the bus guarantee must have a single source", strings.Join(conflicts, ", ")))
	}
	return toolText(f.staleNotes() + chains.Build(fleet, brokers).Render())
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
		// Provenance first, then the per-finding witness lines — byte-for-byte the
		// disclosure the CLI `fitness` command prints. Dropping the substrate line and
		// the caveats (as the MCP tool used to) let an unsound-substrate pass answer a
		// bare "all invariants hold" to the agent loop — a clean-green laundering
		// channel (H-9). GateCaveats is the ONE assembly the CLI/SARIF surfaces share.
		b.WriteString(graph.ProvenanceLine(ix.Algo(), ix.GateCaveats(p.Substrate)))
		for _, f := range res.Violations() {
			writeFinding(&b, "⛔", f)
		}
		for _, f := range res.Cautions() {
			writeFinding(&b, "⚠️ ", f)
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
		s.computeImpeach() // the graph changed; re-audit against the (unchanged) corpus
		return toolText("graph reloaded from " + s.path)
	case "ground":
		card, err := ground.For(ix, p, a.FQN)
		if err != nil {
			return toolError(err.Error())
		}
		return withStale(toolText(card.Render()))
	case "reach":
		// NOTE (M-11): this MCP `reach` renders the impact blast-radius card
		// (impact.ForNodes) — DELIBERATELY a different view than the CLI `reach`
		// command, which prints a bespoke bidirectional callers/callees/cover/effects
		// report. They share a name but are distinct lenses on purpose; the shared
		// resolution logic that DID drift (triage) is unified via resolveTriage above.
		if !ix.Has(a.FQN) {
			return toolError(fmt.Sprintf("no function %q in graph", a.FQN))
		}
		return withStale(toolText(impact.ForNodes(ix, []string{a.FQN}).Render()))
	case "annotate":
		return withStale(annotateCard(ix, a))
	case "triage":
		// resolveTriage/renderTriage are shared with the CLI `triage` command so the
		// two surfaces dispatch the symptom, demand exactly one, and render the
		// disclosure identically (M-11).
		res, card, err := resolveTriage(ix, triageSymptom{Frame: a.Frame, Route: a.Route, Table: a.Table, Event: a.Event, Peer: a.Peer, Fail: a.Fail})
		if err != nil {
			return toolError(err.Error())
		}
		return withStale(toolText(renderTriage(res, card)))
	case "exceptions":
		if p == nil {
			return toolError("the server was started without --policy; exceptions needs one")
		}
		xs := fitness.Exceptions(p, ix)
		ls := fitness.Liveness(p, ix)
		if len(xs) == 0 && len(ls) == 0 {
			return withStale(toolText("no allow-list entries or pattern-bearing rules configured"))
		}
		var b strings.Builder
		for _, x := range xs {
			fmt.Fprintln(&b, x)
		}
		for _, l := range ls {
			fmt.Fprintln(&b, l)
		}
		fmt.Fprintf(&b, "\n%d dead exception(s), %d dead rule pattern(s)\n", fitness.DeadCount(xs), fitness.DeadPatternCount(ls))
		return withStale(toolText(b.String()))
	case "impeach":
		if len(s.corpus) == 0 {
			return toolError("the server was started without --corpus; impeach needs a committed behavioral corpus to audit the graph against")
		}
		if p == nil {
			return toolError("the server was started without --policy; impeach integrates witnesses against the policy's must_not_reach rules and needs one")
		}
		return withStale(toolText(s.impeachBody)) // precomputed at load/reload; staleNote stays dynamic
	default:
		return toolError("unknown tool: " + name)
	}
}

// annotateCard validates a proposed blind-spot annotation against this graph's live
// manifest and returns the ready-to-commit .flowmap.yaml snippet. It is READ-ONLY:
// it writes nothing (the server's NO WRITE TOOLS invariant holds — the agent
// persists the snippet itself), and an annotation is disclosure-only, so even once
// committed it cannot move a count or a verdict. The binding rule is
// config.ResolveAnnotationKind, shared with the producer-side merge.
//
// Parity with the build is exact, in both directions and including the §22
// algorithm-fragility relaxation: a proposal this tool ACCEPTS (binds) is one the
// build binds, a proposal it hard-REJECTS (orphan/stale FQN, ambiguous multi-kind
// site, or a mismatch on an algo-STABLE kind) is one the build fails on, and a
// proposal it returns as algorithm-dependent GUIDANCE (a fragile kind absent under
// this graph's --algo at an otherwise-live site) is exactly the case the build
// warn-and-skips rather than failing. The tool therefore never rejects what the
// build would tolerate, nor vice versa.
func annotateCard(ix *graph.Index, a toolArgs) map[string]any {
	if strings.TrimSpace(a.Site) == "" {
		return toolError("site is required: the FQN of the blind spot to annotate")
	}
	if strings.TrimSpace(a.Note) == "" {
		return toolError("note is required: the context to attach")
	}
	spots := ix.BlindSpotsAt(a.Site)
	kinds := make([]string, 0, len(spots))
	for _, b := range spots {
		kinds = append(kinds, b.Kind)
	}
	kind, err := config.ResolveAnnotationKind(a.Site, a.Kind, kinds)
	if err != nil {
		// Mirror the producer-side merge (graphio.mergeAnnotations): a requested kind
		// merely ABSENT under this graph's --algo at an otherwise-live site, and itself
		// algorithm-fragile, is NOT a hard rejection — the build warn-and-skips it, never
		// fails (§22). Return algorithm-dependent guidance instead, so the tool and the
		// build agree on this case. Every other error — an orphan (stale FQN), an
		// ambiguous multi-kind site, or a mismatch on an algo-stable kind (a real typo) —
		// stays a hard rejection, fail-closed, exactly as the build still fails on it.
		var ka *config.KindAbsentError
		if errors.As(err, &ka) && blindspots.AlgoFragile(blindspots.Kind(ka.RequestedKind)) {
			return toolText(fmt.Sprintf(
				"the %q blind spot is algorithm-dependent and is not present at %s under this graph's --algo %s (present: %s).\n"+
					"A flowmap build would warn-and-skip — never fail — an annotation for it here, so committing one is safe but it will NOT attach under %s.\n"+
					"To author and ground it, re-run this server on a graph built with the --algo that surfaces it (e.g. vta), then annotate again.",
				ka.RequestedKind, ka.Site, ix.Algo(), strings.Join(ka.Present, ", "), ix.Algo()))
		}
		return toolError(err.Error())
	}
	// Ground the proposal in the matched disclosure so the author sees what the
	// machine actually saw at this seam.
	var detail string
	for _, b := range spots {
		if b.Kind == kind {
			detail = b.Detail
			break
		}
	}
	var w struct {
		Static struct {
			Annotations []config.Annotation `yaml:"annotations"`
		} `yaml:"static"`
	}
	w.Static.Annotations = []config.Annotation{{Site: a.Site, Kind: kind, Note: a.Note, By: a.By, Claim: a.Claim}}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2) // match the 2-space convention of a hand-written .flowmap.yaml
	if err := enc.Encode(w); err != nil {
		return toolError("rendering annotation YAML: " + err.Error())
	}
	_ = enc.Close()

	var b strings.Builder
	fmt.Fprintf(&b, "✓ binds the %s blind spot at %s\n", kind, a.Site)
	if detail != "" {
		fmt.Fprintf(&b, "  %s\n", detail)
	}
	// Flag an algorithm-fragile kind (§22): its presence at a site flips with --algo,
	// so this binding holds under THIS graph's algo but a flowmap build under a
	// different --algo will warn-and-skip the annotation (never fail). Disclosure-only,
	// so the caller is told to pin --algo, not that anything is wrong.
	if blindspots.AlgoFragile(blindspots.Kind(kind)) {
		fmt.Fprintf(&b, "⚠ %s is algorithm-dependent: present under this graph's --algo %s but a build under a different --algo would warn-and-skip this annotation (it never fails the build). Author it against — and build with — a consistent --algo (e.g. vta).\n", kind, ix.Algo())
	}
	b.WriteString("\nThis tool writes nothing. Add to .flowmap.yaml (merge under an existing static.annotations if present) and rebuild the graph:\n\n")
	b.WriteString(buf.String())
	return toolText(b.String())
}

// computeImpeach renders the audit-only impeachment disclosure for this service and
// caches it in s.impeachBody: the loaded graph joined against its committed corpus,
// resolved against the policy's must_not_reach rules. It runs at OriginLive ON
// PURPOSE — the lens NEVER gates, so GateBlockers is structurally empty and the agent
// can never read this as a merge verdict against a possibly-local graph; the gate is
// verify --corpus over CI graphs (§13 crack #2). Every candidate is disclosed with
// its ladder verdict regardless, so "observe-first" holds.
//
// Computed ONCE (here, called when inputs are final and on every reload) rather than
// per call: the body is a pure function of (graph, corpus, capture, rules) — all
// immutable between reloads — so the same inputs yield byte-identical text, and
// re-running the graph-reachability BFS + corpus hash on each read-locked call would
// be wasted work repeated across concurrent clients. A no-op until both corpus and
// policy are present (the lens is unconfigured otherwise); the per-call staleNote is
// added at call time, never cached.
func (s *mcpServer) computeImpeach() {
	if len(s.corpus) == 0 || s.p == nil {
		s.impeachBody = ""
		return
	}
	p := s.p
	prov := impeach.Provenance{TraceIdentity: s.ix.Stamp(), Capture: s.capture}
	r := impeach.Audit(p.Service, s.ix, s.corpus, prov)
	res := impeach.Resolve(r, s.ix, p.MustNotReach, impeach.OriginLive)
	var b strings.Builder
	fmt.Fprintf(&b, "behavioral audit (AUDIT-ONLY, never a gate) — corpus %s", res.CorpusDigest)
	if res.CaptureProvenance != "" {
		fmt.Fprintf(&b, ", capture %s", res.CaptureProvenance)
	} else {
		b.WriteString(", capture UNGRADED (caps every candidate below IMPEACHMENT)")
	}
	b.WriteString("\n")
	if len(res.Candidates) == 0 {
		b.WriteString("no impeachment candidates: every observed effect is statically accounted for, within the disclosed scope and blind spots\n")
	}
	for _, w := range res.Candidates {
		fmt.Fprintf(&b, "\n• %s  %s\n", w.Verdict, w.Effect)
		fmt.Fprintf(&b, "    observed in flow %q", w.Observed.Flow)
		if w.Observed.Entry != "" {
			fmt.Fprintf(&b, " from %s", w.Observed.Entry)
		}
		b.WriteString("\n")
		if w.Severance != nil && w.Severance.Site != "" {
			disclosed := "UNDISCLOSED blind spot"
			if w.Severance.FrontierKnown {
				disclosed = "disclosed seam"
			}
			fmt.Fprintf(&b, "    static lost it at: %s (%s, %s)\n", w.Severance.Site, w.Severance.Kind, disclosed)
		}
		if len(w.Claim.Rules) > 0 {
			fmt.Fprintf(&b, "    touches must_not_reach rule(s): %s\n", strings.Join(w.Claim.Rules, ", "))
		}
	}
	for _, c := range res.Caveats {
		fmt.Fprintf(&b, "\n⚠️ %s", c)
	}
	if len(res.Caveats) > 0 {
		b.WriteString("\n")
	}
	// Blind-spot annotations vs the corpus: grade each annotation by whether the
	// corpus independently witnesses an effect severed at its site. The corpus
	// witnesses the SEAM, never the note's prose — so a witnessed annotation is a
	// stronger DISCLOSURE (the gap it explains is behaviorally real), never a proof
	// and never a gate. Audit-only, like the rest of this lens.
	if anns := s.ix.Annotations(); len(anns) > 0 {
		b.WriteString("\nblind-spot annotations vs corpus (the corpus witnesses the SEAM/CLAIM, not the note's prose):\n")
		for _, ga := range impeach.GradeAnnotations(res.Candidates, anns) {
			fmt.Fprintf(&b, "  %s %s at %s\n      note: %s\n", annotationMark(ga.Grade), ga.Annotation.Kind, ga.Annotation.Site, ga.Annotation.Note)
			if ga.Annotation.Claim != "" {
				fmt.Fprintf(&b, "      claims: %s\n", ga.Annotation.Claim)
			}
			for _, e := range ga.Effects {
				fmt.Fprintf(&b, "      corpus observed an effect severed here (%s): %s [flow %q]\n", e.Verdict, e.Effect, e.Flow)
			}
			switch ga.Grade {
			case impeach.AnnotationUnwitnessed:
				b.WriteString("      no corpus effect severed at this site; the note stands but is unverified\n")
			case impeach.AnnotationUnconfirmed:
				b.WriteString("      the seam is witnessed but the CLAIMED effect was not observed — look (a sample is not proof of absence)\n")
			}
		}
	}
	// Disclose the load-once contract so corpus freshness is legible, not silent
	// (the graph alone is staleness-tracked; a committed corpus is a startup input).
	// CorpusDigest above IS the agent's integrity check: recompute over the dir to
	// detect drift since this server loaded it.
	if s.corpusDir != "" {
		fmt.Fprintf(&b, "\ncorpus: %d golden(s) under %s, loaded at startup — restart to refresh (the digest above pins the exact set audited)\n", len(s.corpus), s.corpusDir)
	}
	b.WriteString("\nThis lens DISCLOSES; it does not gate. The deterministic merge gate is `groundwork verify --corpus` over CI-built base/branch graphs.\n")
	s.impeachBody = b.String()
}

// annotationMark renders the glyph + fixed-width label for an annotation grade, so
// the audit lines align and the strongest signal (a claim corroborated or not) reads
// at a glance. CONFIRMED/WITNESSED corroborate; UNCONFIRMED flags a discrepancy to
// look at (never "false"); UNWITNESSED is simply unverified.
func annotationMark(g impeach.AnnotationGrade) string {
	switch g {
	case impeach.AnnotationConfirmed:
		return "✓ CONFIRMED  "
	case impeach.AnnotationWitnessed:
		return "✓ WITNESSED  "
	case impeach.AnnotationUnconfirmed:
		return "✗ UNCONFIRMED"
	default:
		return "? UNWITNESSED"
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
