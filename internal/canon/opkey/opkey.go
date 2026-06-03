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

// Canonical op-key prefixes. Of emits keys beginning with these. Any consumer
// that must classify an op (the system-context graph, coverage) should match on
// these constants rather than re-typing the literals, so the rendered grammar
// has one source of truth and a change here can't silently desync a consumer.
// The opkey tests pin that Of's output actually begins with each.
const (
	HTTPPrefix    = "HTTP "
	DBPrefix      = "DB "
	RPCPrefix     = "RPC "
	PublishPrefix = "PUBLISH "
	ConsumePrefix = "CONSUME "
	// SettlePrefix keys a consumer-side acknowledgment that drains a message
	// (SQS DeleteMessage, an ack/nack, a visibility extension). It is a distinct
	// broker interaction from CONSUME — receiving a message and acknowledging it
	// are two calls — so it must not collapse into the same op.
	SettlePrefix = "SETTLE "
)

// Of returns the canonical operation key and peer (counterparty lifeline) for a
// span. name is the raw span name, used only as the fallback identity for an
// internal operation that carries no boundary attributes. Peer is "Bus" for
// producer/consumer spans, the peer service or db system for outbound calls, and
// "" for internal work.
func Of(kind ir.Kind, attrs map[string]string, name string) (op, peer string) {
	// A broker interaction is recognized from its messaging.* attributes before
	// the kind switch, because SDK instrumentation for managed brokers (notably
	// the AWS SDK for SNS/SQS) models the call as a CLIENT-kind RPC span that
	// nonetheless carries messaging.destination.name and messaging.operation. The
	// destination + operation are the identity; classifying by kind alone would
	// render an SNS publish as a bare HTTP/RPC call to "sns" and merge an SQS
	// receive with its delete.
	if op, ok := messaging(kind, attrs); ok {
		return op, brokerPeer(attrs)
	}
	switch kind {
	case ir.KindServer:
		return httpKey("HTTP", method(attrs), "", route(attrs)), ""
	case ir.KindClient:
		if sys := rpcSystem(attrs); sys != "" || first(attrs, "rpc.service") != "" {
			return rpcKey(attrs), rpcPeer(attrs)
		}
		if sys := dbSystem(attrs); sys != "" {
			return dbKey(sys, attrs), dbPeer(sys, attrs)
		}
		p := first(attrs, "peer.service", "server.address")
		return httpKey("HTTP", method(attrs), p, route(attrs)), p
	case ir.KindProducer:
		return PublishPrefix + destination(attrs), brokerPeer(attrs)
	case ir.KindConsumer:
		return ConsumePrefix + destination(attrs), brokerPeer(attrs)
	default: // internal
		if sys := dbSystem(attrs); sys != "" {
			return dbKey(sys, attrs), dbPeer(sys, attrs)
		}
		return name, ""
	}
}

// brokerPeer is the counterparty lifeline for a broker interaction, canonicalized
// from messaging.system so distinct messaging systems do not collapse into one
// "Bus" lifeline. A span carrying no messaging.system (the common in-process
// event-bus shape) keeps the generic "Bus", so existing single-bus diagrams are
// unchanged; a managed system (SNS, SQS, Kafka, …) names its own lifeline, so an
// SNS publish and an event-bus consume no longer share a participant. An
// unrecognized system uses its raw name rather than masquerading as the default
// bus.
func brokerPeer(attrs map[string]string) string {
	switch strings.ToLower(first(attrs, "messaging.system")) {
	case "", "event_bus", "eventbus":
		return "Bus"
	case "aws_sns", "sns":
		return "SNS"
	case "aws_sqs", "sqs":
		return "SQS"
	case "kafka", "apache_kafka":
		return "Kafka"
	case "rabbitmq":
		return "RabbitMQ"
	default:
		return first(attrs, "messaging.system")
	}
}

// dbPeer qualifies the database system with the specific database instance — the
// logical database (db.namespace) or, failing that, the server host
// (server.address) — so two databases of the same system render as distinct
// lifelines (postgresql/event_bus_test vs postgresql/cgate_test) instead of
// collapsing into one "postgresql" participant that conflates two stores. The
// bare system is the peer when neither qualifier is present, so a span carrying
// no instance detail (the common fixture and in-process shape) is unchanged.
func dbPeer(system string, attrs map[string]string) string {
	ns := first(attrs, "db.namespace")
	if ns == "" {
		ns = first(attrs, "server.address")
	}
	if ns == "" {
		return system
	}
	return system + "/" + ns
}

// EffectiveKind is the messaging role a span plays, derived from its messaging.*
// attributes when present and falling back to the raw span kind otherwise. It is
// the single normalization that lets every kind-keyed consumer — the boundary-
// effect gate, the system-context graph, the renderer, tiering — treat an AWS
// SDK SNS/SQS CLIENT span as the producer/consumer it behaviorally is, without
// each re-deriving the messaging role. A publish or settle is an outbound broker
// interaction (KindProducer); a receive/process is inbound (KindConsumer).
func EffectiveKind(kind ir.Kind, attrs map[string]string) ir.Kind {
	if _, ok := messaging(kind, attrs); !ok {
		return kind
	}
	if messagingDirection(kind, attrs) == dirConsume {
		return ir.KindConsumer
	}
	return ir.KindProducer
}

// BusDestination strips the PUBLISH/CONSUME/SETTLE prefix from a messaging op
// key, yielding the bare destination (topic/queue) for edge labeling.
func BusDestination(op string) string {
	for _, p := range []string{PublishPrefix, ConsumePrefix, SettlePrefix} {
		if strings.HasPrefix(op, p) {
			return strings.TrimPrefix(op, p)
		}
	}
	return op
}

// IsSettle reports whether op is a consumer-side acknowledgment (drain), which a
// choreography view must not treat as a publish.
func IsSettle(op string) bool { return strings.HasPrefix(op, SettlePrefix) }

// msgDir is the direction of a broker interaction.
type msgDir int

const (
	dirPublish msgDir = iota
	dirConsume
	dirSettle
)

// messaging returns the canonical op key for a broker interaction, or ok=false
// when the span carries no messaging destination. The destination is required —
// it is the identity of the interaction — so a non-messaging span never matches.
func messaging(kind ir.Kind, attrs map[string]string) (string, bool) {
	// An inbound entry (server) is never a broker interaction, even if it carries
	// a stray messaging.destination: reclassifying it would turn an HTTP/RPC entry
	// into a published event. Producer, consumer, client (the AWS-SDK shape), and
	// internal spans can all be broker calls.
	if kind == ir.KindServer {
		return "", false
	}
	dest := destination(attrs)
	if dest == "" {
		return "", false
	}
	switch messagingDirection(kind, attrs) {
	case dirConsume:
		return ConsumePrefix + dest, true
	case dirSettle:
		return SettlePrefix + dest, true
	default:
		return PublishPrefix + dest, true
	}
}

// messagingDirection classifies the operation from the semantic-convention
// operation value, matched EXACTLY rather than by substring — a control-plane
// "CreateQueue" / "DeleteTopic" must not read as a data-plane publish / settle.
// messaging.operation.name carries lower-level SDK method names that are too
// ambiguous to direction-classify (DeleteMessage vs DeleteQueue both contain
// "delete"), so an operation with no recognized type defers to the span kind,
// which already distinguishes a producer from a consumer.
func messagingDirection(kind ir.Kind, attrs map[string]string) msgDir {
	switch strings.ToLower(first(attrs, "messaging.operation.type", "messaging.operation")) {
	case "publish", "send", "create", "produce", "enqueue":
		return dirPublish
	case "receive", "process", "poll", "deliver", "consume":
		return dirConsume
	case "settle", "ack", "nack", "delete", "complete", "abandon", "reject", "extend":
		return dirSettle
	}
	if kind == ir.KindConsumer {
		return dirConsume
	}
	return dirPublish
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
		return RPCPrefix + svc + "/" + m
	case svc != "":
		return RPCPrefix + svc
	default:
		return RPCPrefix + m
	}
}

// rpcPeer is the counterparty lifeline for an RPC client call. It prefers an
// explicit rpc.service; failing that, the AWS SDK encodes "Service/Operation" in
// rpc.method with no separate rpc.service (SQS/ReceiveMessage), and the service
// prefix is the meaningful peer (SQS, SNS) — not the transport host
// (server.address = the LocalStack/endpoint URL), which is the bare-HTTP
// fallback we are specifically avoiding for AWS-SDK spans.
func rpcPeer(attrs map[string]string) string {
	// Prefer an explicit service, then a declared peer; only then the AWS
	// "Service/Operation" prefix in rpc.method (SQS/ReceiveMessage -> SQS), and
	// last the transport host. peer.service ahead of the method prefix keeps a
	// non-AWS RPC span that names its peer unchanged; the method prefix ahead of
	// server.address lands an AWS call on the service (SQS), not the endpoint host.
	if svc := first(attrs, "rpc.service", "peer.service"); svc != "" {
		return svc
	}
	if m := first(attrs, "rpc.method"); m != "" {
		if i := strings.IndexByte(m, '/'); i > 0 {
			return m[:i]
		}
	}
	return first(attrs, "server.address")
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

// rpcSystem reads the RPC system, accepting both the original rpc.system and the
// newer rpc.system.name spelling (AWS SDK instrumentation emits the latter), so
// an RPC client call is not misclassified as a bare HTTP call when only the
// newer attribute is present.
func rpcSystem(attrs map[string]string) string {
	return first(attrs, "rpc.system", "rpc.system.name")
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
