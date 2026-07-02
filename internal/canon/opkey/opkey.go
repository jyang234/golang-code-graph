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
	"regexp"
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
func Of(kind ir.Kind, attrs map[string]string, name string, options ...Options) (op, peer string) {
	o := opts(options)
	// A broker interaction is recognized from its messaging.* attributes before
	// the kind switch, because SDK instrumentation for managed brokers (notably
	// the AWS SDK for SNS/SQS) models the call as a CLIENT-kind RPC span that
	// nonetheless carries messaging.destination.name and messaging.operation. The
	// destination + operation are the identity; classifying by kind alone would
	// render an SNS publish as a bare HTTP/RPC call to "sns" and merge an SQS
	// receive with its delete.
	if op, ok := messaging(kind, attrs, o.ShortHexIDs); ok {
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
		// peer.service (logical) > server.address (current semconv host) > net.peer.name
		// (legacy semconv host, emitted by otelhttp ≤0.57). Without the legacy spelling a
		// client span carrying only net.peer.name yields an empty peer and renders as a
		// self-edge.
		p := first(attrs, "peer.service", "server.address", "net.peer.name")
		return httpKey("HTTP", method(attrs), p, route(attrs)), p
	case ir.KindProducer:
		return PublishPrefix + normalizeDestination(destination(attrs), o.ShortHexIDs), brokerPeer(attrs)
	case ir.KindConsumer:
		return ConsumePrefix + normalizeDestination(destination(attrs), o.ShortHexIDs), brokerPeer(attrs)
	default: // internal
		if sys := dbSystem(attrs); sys != "" {
			return dbKey(sys, attrs), dbPeer(sys, attrs)
		}
		return name, ""
	}
}

// Options tunes op-key derivation. The zero value is the conservative default.
type Options struct {
	// ShortHexIDs additionally templates short (8–15 char) hex id tokens in messaging
	// destination labels, beyond the always-on UUID / numeric / long-hex templating.
	// Opt-in because a short hex token is ambiguous with a stable name segment; enable
	// it for instrumentation whose topic/queue names bake first-party ids shorter than
	// a UUID (e.g. eb-dev-evt-fddd7c99-v1).
	ShortHexIDs bool
}

func opts(o []Options) Options {
	if len(o) > 0 {
		return o[0]
	}
	return Options{}
}

// brokerPeer is the counterparty lifeline for a broker interaction, canonicalized
// from messaging.system. Only a span carrying NO messaging.system gets the generic
// "Bus" — there is nothing to name it after. A named system keeps a stable
// identifying name: a friendly label for managed infrastructure that is never itself
// a modeled service (SNS, SQS, Kafka, RabbitMQ), and otherwise the raw system name.
// Keeping the raw name is deliberate: a first-party event bus that is also a service
// in the flow (messaging.system == its service.name) then coincides with that service
// participant by name — the broker is drawn as the real downstream rather than a
// synthetic node that duplicates it. Distinct managed systems still get distinct
// lifelines, so an SNS publish and an SQS receive never collapse together.
func brokerPeer(attrs map[string]string) string {
	switch strings.ToLower(first(attrs, "messaging.system")) {
	case "":
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
	if !isMessaging(kind, attrs) {
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

// isMessaging reports whether a span is a broker interaction: a non-server span with
// a messaging destination. An inbound entry (server) is never one, even with a stray
// messaging.destination — reclassifying it would turn an HTTP/RPC entry into a
// published event; producer, consumer, client (the AWS-SDK shape), and internal spans
// can all be broker calls. The destination is required — it is the identity of the
// interaction. This is the cheap classification EffectiveKind needs, without building
// or normalizing the op key.
func isMessaging(kind ir.Kind, attrs map[string]string) bool {
	return kind != ir.KindServer && destination(attrs) != ""
}

// messaging returns the canonical op key for a broker interaction, or ok=false
// when the span carries no messaging destination.
func messaging(kind ir.Kind, attrs map[string]string, shortHexIDs bool) (string, bool) {
	if !isMessaging(kind, attrs) {
		return "", false
	}
	dest := normalizeDestination(destination(attrs), shortHexIDs)
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

// DBOperation derives the DB operation verb from a span's attributes with the
// canonical precedence: an explicit db.operation / db.operation.name attribute
// wins; absent that, the verb is read off the normalized statement. It is returned
// UPPER-CASED so it keys and classifies identically regardless of the
// instrumentation's casing (sql.Normalize already upper-cases the statement path).
// "" when no attribute names it and the statement is too opaque to read a verb.
//
// This is the ONE operation-derivation both the op-key builder (dbKey, below) and
// the canon effect classifier (canon.dbOperation) call, so the two can never
// disagree about which verb a DB span performed — a divergence would split the
// behavioral-impeachment join or mis-tier a read as a mutation (M-10, one source
// of truth). Parity is pinned by TestDBOperationParity in the canon package.
func DBOperation(attrs map[string]string) string {
	// needTable=false: the verb alone is wanted, so the statement is normalized only
	// when no operation attribute is present — never just to fill an unused table.
	op, _ := dbOpAndTable(attrs, false)
	return op
}

// dbOpAndTable derives the DB operation verb and, when needTable is set, the
// primary table from a span's attributes, normalizing the statement AT MOST ONCE.
// The verb precedence (db.operation / db.operation.name attribute, else the
// normalized statement) lives here so DBOperation and dbKey share it (M-10) without
// paying for two sql.Normalize passes over the same statement on the canon hot path
// (the common raw-SQL span carries neither a db.operation nor a db.sql.table
// attribute, so op and table would each have triggered their own normalization).
//
// The verb is UPPER-CASED and the attribute-supplied table LOWER-CASED so both key
// identically to the statement-derived forms (sql.Normalize upper-cases the verb
// and lower-cases the table): otherwise db.sql.table:"Applicants" and a
// statement-derived "applicants" would mint two op keys for one table, splitting
// the impeachment join and churning goldens on an instrumentation upgrade (M-25).
func dbOpAndTable(attrs map[string]string, needTable bool) (op, table string) {
	op = strings.ToUpper(first(attrs, "db.operation", "db.operation.name"))
	table = strings.ToLower(first(attrs, "db.sql.table", "db.collection.name"))
	if op == "" || (needTable && table == "") {
		if stmt := statement(attrs); stmt != "" {
			n := sql.Normalize(stmt)
			if op == "" {
				op = n.Operation
			}
			if table == "" {
				table = n.Table
			}
		}
	}
	return op, table
}

// dbKey assembles "DB <system> <OPERATION> <table>", keyed on operation and
// table so identity barely depends on the statement text (canon §8.3).
func dbKey(system string, attrs map[string]string) string {
	op, table := dbOpAndTable(attrs, true)
	parts := []string{"DB", system}
	if op != "" {
		parts = append(parts, op)
	}
	if table != "" {
		parts = append(parts, table)
	}
	return strings.Join(parts, " ")
}

// ParseDBKey is the inverse of dbKey: it splits a canonical database op key
// ("DB <system> <OP> <table>") back into its parts. Kept here, beside dbKey, so
// the format has ONE owner — a consumer that needs the structured DB identity of
// an already-canonicalized span (the behavioral-impeachment join, which reconciles
// this against the static "boundary:db <OP> <table>" label) parses through this
// rather than re-splitting the string itself and drifting from dbKey's grammar.
// ok is false for any op that is not a DB key. operation/table are "" when the
// underlying statement was too opaque for dbKey to name them (it omits absent
// parts), so a caller must treat an empty operation as unreadable, never a write.
func ParseDBKey(op string) (system, operation, table string, ok bool) {
	rest, found := strings.CutPrefix(op, DBPrefix)
	if !found {
		return "", "", "", false
	}
	f := strings.Fields(rest)
	switch len(f) {
	case 0:
		return "", "", "", false
	case 1:
		return f[0], "", "", true
	case 2:
		return f[0], f[1], "", true
	default:
		// table identifiers do not carry spaces, but join defensively so a
		// surprising multi-token tail round-trips rather than being truncated.
		return f[0], f[1], strings.Join(f[2:], " "), true
	}
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
	return first(attrs, "server.address", "net.peer.name")
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

// destUUID matches a canonical UUID, which embeds '-' and so must be collapsed
// before a destination is split on its delimiters.
var destUUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// normalizeDestination parameterizes the volatile ids embedded in a messaging
// destination (topic/queue) name, the same way url.Template does for a route path,
// so a per-run id baked into the name does not churn the op key (and the gated
// boundary effect). By default it templates only the unambiguous cases — a full
// UUID, and each token url.IsID recognizes (all-numeric or a 16+ hex run) — so
// cgate-email:<uuid> -> cgate-email:{id} while a stable name is untouched. When
// shortHexIDs is set (an opt-in for instrumentation whose names bake first-party ids
// shorter than a UUID) it also templates an 8–15 char hex token containing a digit,
// so eb-dev-evt-fddd7c99-v1 -> eb-dev-evt-{id}-v1.
func normalizeDestination(d string, shortHexIDs bool) string {
	if d == "" {
		return d
	}
	d = destUUID.ReplaceAllString(d, "{id}")
	var b strings.Builder
	tokStart := 0
	flush := func(end int) {
		if end <= tokStart {
			return
		}
		tok := d[tokStart:end]
		if url.IsID(tok) || (shortHexIDs && isShortHexID(tok)) {
			b.WriteString("{id}")
		} else {
			b.WriteString(tok)
		}
	}
	for i := 0; i < len(d); i++ {
		if isDestDelim(d[i]) {
			flush(i)
			b.WriteByte(d[i])
			tokStart = i + 1
		}
	}
	flush(len(d))
	return b.String()
}

// isDestDelim reports whether c separates the structural parts of a destination name.
func isDestDelim(c byte) bool {
	switch c {
	case '-', '_', '.', ':', '/', '~':
		return true
	}
	return false
}

// isShortHexID reports whether a token is a short (8–15 char) hex run containing a
// digit — a first-party id shorter than a UUID (e.g. an event id fddd7c99),
// distinguished from a hex-looking word like "deadbeef" by requiring a digit. Full
// UUIDs and 16+ hex runs are already templated by the conservative default, so this
// covers only the 8–15 range, and only under the opt-in.
func isShortHexID(s string) bool {
	if len(s) < 8 || len(s) > 15 {
		return false
	}
	hasDigit := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			hasDigit = true
		case c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
			// hex letter
		default:
			return false
		}
	}
	return hasDigit
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
