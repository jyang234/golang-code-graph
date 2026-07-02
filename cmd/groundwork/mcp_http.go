package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// serveMCPHTTP is the Tier 3 transport: the same fleet, served over MCP
// streamable HTTP (protocol revision 2025-03-26) so one centrally-managed
// server — fed directly by CI artifacts — answers a whole team's agents.
// This STRENGTHENS the trust posture: with stdio, the agent's .mcp.json
// picks the file the server loads (claim-chain trust); here the operator
// picked the inputs and the agent cannot choose them at all.
//
// The two costs the plan named, paid minimally:
//   - auth: one static bearer token (--token / $GROUNDWORK_MCP_TOKEN),
//     compared in constant time, REQUIRED when binding off loopback —
//     an unauthenticated team server fails at startup, not in production.
//     TLS is a reverse proxy's job; this binary stays deployment-shaped
//     only to the extent it must.
//   - lifecycle: graceful drain on SIGINT/SIGTERM.
//
// The server is deliberately stateless (the spec makes sessions optional):
// every POST carries one JSON-RPC message and gets one JSON response. No
// SSE streams are offered — no tool here ever sends a server-initiated
// message, so GET is 405, honestly, rather than an idle stream.
func serveMCPHTTP(addr, token string, fleet *mcpFleet) error {
	if err := guardHTTPExposure(addr, token); err != nil {
		return err
	}
	fleet.proto = "2025-03-26"
	srv := &http.Server{
		Addr:              addr,
		Handler:           fleet.httpHandler(token),
		ReadHeaderTimeout: 10 * time.Second,
		// Bound a slow or stuck client so it cannot pin a connection open: this is a
		// non-streaming request/response server (no SSE), so generous but finite
		// whole-request and idle deadlines are safe and close the slowloris footgun.
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	fmt.Fprintf(os.Stderr, "groundwork mcp: serving %d service(s) at http://%s/mcp\n", len(fleet.names), addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// guardHTTPExposure fails startup when the bind address is reachable beyond
// loopback and no token is set. Fail-closed at the moment of configuration:
// the operator who exposes the server is the one who must hold the secret.
func guardHTTPExposure(addr, token string) error {
	if token != "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--http wants host:port, got %q: %w", addr, err)
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("--http %s binds beyond loopback without a token; set --token or $GROUNDWORK_MCP_TOKEN (or bind 127.0.0.1)", addr)
	}
	return nil
}

// isLoopbackHost is the one definition of "loopback" the exposure guard and
// the Origin defense share — the two must never drift apart. An empty host
// (":8137", all interfaces) and an unresolved hostname are NOT loopback:
// both checks fail closed.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// httpHandler is the streamable-HTTP endpoint plus /healthz. Auth failures
// and transport misuse are HTTP errors; everything past the front door is
// the same dispatch the stdio loop uses, with the same isError tool results.
func (f *mcpFleet) httpHandler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok\n") // liveness only; answers need auth
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		// The spec's DNS-rebinding defense: a browser-borne request carries
		// the attacker's Origin; only loopback origins may speak to us.
		if o := r.Header.Get("Origin"); o != "" && !loopbackOrigin(o) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if token != "" {
			// Require the Bearer scheme explicitly (a bare token without it is
			// rejected), then compare SHA-256 digests rather than the raw tokens:
			// the digests are fixed-length, so ConstantTimeCompare never
			// short-circuits on a length mismatch — which would leak the token's
			// length through timing.
			presented, hasScheme := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			want := sha256.Sum256([]byte(token))
			got := sha256.Sum256([]byte(presented))
			if !hasScheme || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "stateless MCP server: POST one JSON-RPC message (no SSE streams, no sessions)", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<24))
		var req rpcRequest
		if err != nil || json.Unmarshal(body, &req) != nil {
			http.Error(w, "bad request: one JSON-RPC 2.0 object per POST (batches not supported)", http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted) // a notification: acknowledged, no body
			return
		}
		// Session identity is transport-scoped: initialize mints an id and
		// hands it back as Mcp-Session-Id; clients echo it on later requests.
		// It is a transcript attribution label ONLY — the server stores no
		// session state, never requires the header, and a client that omits
		// it simply lands in the transcript's anonymous bucket. This is what
		// keeps the shared team log readable: concurrent clients interleave
		// lines, and attribution rides the id, not the line order.
		session := r.Header.Get("Mcp-Session-Id")
		if req.Method == "initialize" {
			session = f.newSession()
			w.Header().Set("Mcp-Session-Id", session)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.dispatch(req, session))
	})
	return mux
}

// loopbackOrigin reports whether an Origin header names a loopback host.
func loopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return isLoopbackHost(u.Hostname())
}
