// Package opkey derives flowmap's canonical operation key from a span's kind and
// attributes (canon spec §3.5). Raw span names encode ids and vary across
// instrumentation libraries, so identity is *derived* — HTTP POST
// /loan-application, DB postgresql SELECT applicants, PUBLISH loan.approved —
// decoupling the snapshot from naming quirks.
//
// The same derivation backs two consumers: the canonicalizer (which sets Op and
// Peer on every span) and the expected-exit marker grammar of the flow DSL
// (harness §4, Phase 6), so a declared marker like "PUBLISH loan.approved"
// matches a canonical Op exactly. Keeping it in one package is what couples the
// marker grammar to the op keys.
package opkey

import (
	"strings"

	"github.com/jyang234/golang-code-graph/internal/canon/sql"
	"github.com/jyang234/golang-code-graph/internal/canon/url"
	"github.com/jyang234/golang-code-graph/ir"
)

// Of returns the canonical operation key and peer (counterparty lifeline) for a
// span. name is the raw span name, used only as the fallback identity for an
// internal operation that carries no boundary attributes. Peer is "Bus" for
// producer/consumer spans, the peer service or db system for outbound calls, and
// "" for internal work.
func Of(kind ir.Kind, attrs map[string]string, name string) (op, peer string) {
	switch kind {
	case ir.KindServer:
		return httpKey("HTTP", method(attrs), "", route(attrs)), ""
	case ir.KindClient:
		if sys := first(attrs, "rpc.system"); sys != "" || first(attrs, "rpc.service") != "" {
			return rpcKey(attrs), first(attrs, "rpc.service", "peer.service", "server.address")
		}
		if sys := dbSystem(attrs); sys != "" {
			return dbKey(sys, attrs), sys
		}
		p := first(attrs, "peer.service", "server.address")
		return httpKey("HTTP", method(attrs), p, route(attrs)), p
	case ir.KindProducer:
		return "PUBLISH " + destination(attrs), "Bus"
	case ir.KindConsumer:
		return "CONSUME " + destination(attrs), "Bus"
	default: // internal
		if sys := dbSystem(attrs); sys != "" {
			return dbKey(sys, attrs), sys
		}
		return name, ""
	}
}

// httpKey assembles "HTTP <METHOD> [<peer> ]<route>". The peer is included for
// outbound client calls (HTTP GET credit-bureau /score/{id}) and omitted for the
// service's own inbound server span (HTTP POST /loan-application).
func httpKey(proto, m, peer, rt string) string {
	parts := []string{proto}
	if m != "" {
		parts = append(parts, m)
	}
	if peer != "" {
		parts = append(parts, peer)
	}
	if rt != "" {
		parts = append(parts, rt)
	}
	return strings.Join(parts, " ")
}

// dbKey assembles "DB <system> <OPERATION> <table>", keyed on operation and
// table so identity barely depends on the statement text (canon §8.3).
func dbKey(system string, attrs map[string]string) string {
	op := strings.ToUpper(first(attrs, "db.operation", "db.operation.name"))
	table := first(attrs, "db.sql.table", "db.collection.name")
	if stmt := statement(attrs); stmt != "" {
		n := sql.Normalize(stmt)
		if op == "" {
			op = n.Operation
		}
		if table == "" {
			table = n.Table
		}
	}
	parts := []string{"DB", system}
	if op != "" {
		parts = append(parts, op)
	}
	if table != "" {
		parts = append(parts, table)
	}
	return strings.Join(parts, " ")
}

// rpcKey assembles "RPC <service>/<method>".
func rpcKey(attrs map[string]string) string {
	svc := first(attrs, "rpc.service")
	m := first(attrs, "rpc.method")
	switch {
	case svc != "" && m != "":
		return "RPC " + svc + "/" + m
	case svc != "":
		return "RPC " + svc
	default:
		return "RPC " + m
	}
}

func method(attrs map[string]string) string {
	return strings.ToUpper(first(attrs, "http.request.method", "http.method"))
}

func route(attrs map[string]string) string {
	if rt := first(attrs, "http.route", "url.template"); rt != "" {
		return url.Template(rt)
	}
	return url.Template(first(attrs, "url.path", "http.target", "url.full"))
}

func destination(attrs map[string]string) string {
	return first(attrs, "messaging.destination.name", "messaging.destination")
}

func dbSystem(attrs map[string]string) string {
	return first(attrs, "db.system", "db.system.name")
}

func statement(attrs map[string]string) string {
	return first(attrs, "db.statement", "db.query.text")
}

// Statement returns the raw SQL statement attribute (either semantic-convention
// spelling), or "" if absent. The canonicalizer uses it to project a normalized
// db.statement into the snapshot's attributes.
func Statement(attrs map[string]string) string { return statement(attrs) }

// first returns the value of the first present, non-empty key.
func first(attrs map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := attrs[k]; v != "" {
			return v
		}
	}
	return ""
}
