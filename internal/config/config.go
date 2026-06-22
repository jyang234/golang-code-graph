// Package config loads and validates the per-service .flowmap.yaml document.
// Phase 1 models the tier layer — classification hints plus tier rules and pins.
// Later phases extend the struct with canonicalization knobs and per-flow
// declarations; unknown keys are rejected so a typo never silently disables a
// policy.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FileName is the per-service config document's name. It is the single source of
// truth for the filename across both pipelines: the static analyzer and the
// behavioral flow runner both discover and load it through this package.
const FileName = ".flowmap.yaml"

// Config mirrors .flowmap.yaml. Tier rules and pins are ordered slices, never
// maps, because map iteration order is non-deterministic and first-match wins.
type Config struct {
	Version  int           `yaml:"version"`
	Service  string        `yaml:"service"` // service name for the boundary contract; defaults to the module's last path segment
	Classify ClassifyHints `yaml:"classify"`

	// Entrypoints declares the roots call-resolution cannot reach (see
	// EntrypointHints). Kept SEPARATE from classify.busConsume on purpose — the two
	// key spaces serve different jobs and overloading one mis-roots the other.
	Entrypoints EntrypointHints `yaml:"entrypoints"`

	// Tier layer (tier-map spec §3b).
	UseDefaults *bool  `yaml:"useDefaults"` // nil => true
	CatchAll    int    `yaml:"catchAll"`    // 0 => default 3
	Rules       []Rule `yaml:"rules"`
	Pins        []Pin  `yaml:"pins"`

	// Canonicalization layer (canon spec §4).
	Canon CanonConfig `yaml:"canon"`

	// Static-analysis layer (static-extractor spec).
	Static StaticConfig `yaml:"static"`

	// Obligation layer (path-obligations plan): domain lifecycle rules evaluated
	// over each function's SSA control-flow graph.
	Obligations []ObligationRule `yaml:"obligations"`

	// Value-flow / taint layer (headroom §3): the declared sensitive sources and
	// must-not-receive sinks the `flowmap taint` analysis runs against.
	Taint TaintConfig `yaml:"taint"`
}

// TaintConfig declares the sources and sinks for the forward value-flow analysis.
// SourceFuncs and Sinks are "importpath#Name" (the classify-hint shape); SourceFields
// is "importpath#Type.Field" — a sensitive struct field whose reads are sources.
type TaintConfig struct {
	SourceFuncs  []string `yaml:"sourceFuncs"`
	SourceFields []string `yaml:"sourceFields"`
	Sinks        []string `yaml:"sinks"`
}

// ObligationRule declares one path obligation, keyed to our named functions —
// the domain-specific lifecycle no off-the-shelf analyzer can know. The kind is
// inferred from which field set is present (exactly one is required):
//
//   - must-release (acquire/release): after a call to Acquire, every path to
//     function exit must hit a call to one of Release (tx commit/rollback,
//     custom resource close). Plain `defer` of a release counts.
//   - must-precede (require/before): every call to Before must be dominated by
//     a call to Require (audit-write before publish, auth before privileged
//     call).
//
// Every reference uses the classify-hint "import/path#Symbol" form, but unlike
// a hint the symbol is required: an obligation anchors on a specific function.
// The symbol matches by name within the package (receiver-agnostic), on both
// concrete call targets and interface-method (invoke) call sites.
type ObligationRule struct {
	Name    string   `yaml:"name"`
	Acquire string   `yaml:"acquire,omitempty"`
	Release []string `yaml:"release,omitempty"`
	Require string   `yaml:"require,omitempty"`
	Before  string   `yaml:"before,omitempty"`

	// FromCallers opts a must-precede rule into the interprocedural entry-
	// domination lift (correctness plan, D-CX7/D-CX9): a Before site with no
	// dominating Require in its own function is SATISFIED when every entry
	// into that function is require-dominated, CANT-PROVE when the entries are
	// beyond proof, VIOLATED (with the entering caller named) when a provably
	// require-less entry exists. Guard-intent rules (auth, validation) want
	// this; pairing-intent rules (an audit per publish) usually do not — an
	// incidental upstream require would satisfy them, so the default is off.
	FromCallers bool `yaml:"fromCallers,omitempty"`
}

// Obligation kinds, inferred from a rule's populated field set.
const (
	KindMustRelease = "must-release"
	KindMustPrecede = "must-precede"
)

// Kind returns the rule's obligation kind. Only meaningful on a validated rule.
func (r *ObligationRule) Kind() string {
	if r.Acquire != "" || len(r.Release) > 0 {
		return KindMustRelease
	}
	return KindMustPrecede
}

// StaticConfig holds knobs for the static pipeline.
type StaticConfig struct {
	// HighFanOutThreshold is the callee count above which a single dynamic-dispatch
	// site is flagged as likely over-approximation in the (non-gated) graph
	// blind-spots. 0 => the built-in default. Interface-dense services may raise it.
	HighFanOutThreshold int `yaml:"highFanOutThreshold"`

	// Routers declares HTTP route-registration functions root discovery should
	// recognize, beyond the built-in stdlib ServeMux and go-chi. Each entry covers
	// a router whose method is implied by the registration function name and whose
	// handler is a single positional func argument (echo and most custom routers;
	// not gin's variadic handlers or gorilla's chained .Methods()).
	Routers []RouterHint `yaml:"routers"`

	// DeclaredBlindSpots are human-RATIFIED seams (the behavioral-impeachment loop's
	// blind-spot repairs, plan §8): sites where static must ABSTAIN because behavior
	// proved the over-approximation's disclosure incomplete. flowmap merges each into
	// the graph's blind spots so the next run is honest at the seam (NEVER →
	// CANT-PROVE — the safe direction; a declared seam can only WEAKEN proofs, never
	// hide a violation, since reachability is edge-based). This is the ENACTMENT half
	// of the loop: the loop proposes + self-extinguish-verifies a repair, and a
	// CODEOWNER commits the seam here (paired with a blind_spot_ratchet allow-list
	// entry in the policy, §8 crack #6). Each carries a reason — the witness — for audit.
	DeclaredBlindSpots []DeclaredBlindSpot `yaml:"declaredBlindSpots,omitempty"`

	// ExternalBoundaryExempt lists third-party package-path prefixes to treat as
	// infrastructure/plumbing rather than a dependency boundary, suppressing their
	// ExternalBoundaryCall disclosures. OpenTelemetry is exempt built-in (observability
	// plumbing already modeled on the behavioral side); teams add framework/utility
	// deps — an HTTP router, a decimal lib — whose handoffs are not the external
	// surface a reviewer reviews. Matched by package-path prefix at a segment boundary,
	// so "github.com/go-chi/chi/v5" also covers its subpackages. It only NARROWS a
	// disclosure: it can never hide a classified boundary effect (those are excluded by
	// classification, not by this) nor change a verdict (ExternalBoundaryCall is
	// disclosure-only), so an over-broad entry costs visibility, never soundness.
	ExternalBoundaryExempt []string `yaml:"externalBoundaryExempt,omitempty"`

	// ExternalBoundaryTrivial lists third-party package-path prefixes whose
	// ExternalBoundaryCall disclosures are PURE-COMPUTE / framework plumbing (uuid
	// generation, an HTTP router's helpers, a codegen runtime) rather than
	// effect-bearing seams (a cloud-SDK send, a DB driver). Unlike
	// ExternalBoundaryExempt — which SUPPRESSES the disclosure entirely — this only
	// TAGS it as the trivial signal/noise tier: the blind spot is still detected,
	// counted, and rendered (the count is unchanged), so a reader (and a per-tier
	// view) can separate the framework noise from the effect-bearing handful (§21.A).
	// Matched by package-path prefix at a segment boundary, like
	// ExternalBoundaryExempt. Disclosure-only: an over-broad entry mis-PRIORITIZES, it
	// never hides an effect or moves a verdict.
	ExternalBoundaryTrivial []string `yaml:"externalBoundaryTrivial,omitempty"`

	// Annotations attach human/AI CONTEXT to a blind spot the analysis already
	// detected — the irreducible residue a machine-stated shape cannot close (what
	// happens BEHIND an ExternalBoundaryCall, inside a goroutine, past an unresolved
	// func value). They are DISCLOSURE-ONLY: an annotation never closes a blind spot,
	// changes a count, or moves a verdict — it decorates the disclosure channel so a
	// reviewer reads the context, never a laundered claim (CLAUDE.md tenet 3). Each
	// must match a detected blind spot by (kind, site); an annotation that matches
	// none is a load-time error (a stale FQN is drift, fail closed), surfaced where
	// the merge runs (graphio), not silently dropped.
	Annotations []Annotation `yaml:"annotations,omitempty"`

	// SchemaCheck declares the schema source for `flowmap schema-drift`, the
	// deterministic cross-check of code DB-write labels against the migration-defined
	// schema (docs/design/schema-drift-check-plan.md). CLI flags override it. It is
	// DISCLOSURE-ONLY config: schema-drift is a measurement view, not a gate, so an
	// entry here can only change what the check reads, never a verdict.
	SchemaCheck SchemaCheckConfig `yaml:"schemaCheck,omitempty"`
}

// SchemaCheckConfig is the per-service schema-drift configuration. MigrationsDir is
// resolved RELATIVE to the service directory. LibraryOwnedTables names the tables a
// library auto-migrates (the outbox/inbox pattern) that no migration script creates;
// they are folded into the defined schema so they do not false-fire as drift — the
// load-bearing COMPLETENESS condition (a drift flag is sound only against a complete
// schema set).
type SchemaCheckConfig struct {
	MigrationsDir      string   `yaml:"migrationsDir,omitempty"`
	LibraryOwnedTables []string `yaml:"libraryOwnedTables,omitempty"`
}

// Annotation is one piece of human/AI context on a detected blind spot. Site is
// the FQN (fn.RelString(nil)) the disclosure names; Kind, when set, must name a
// recognized blindspots.Kind and disambiguates a site carrying more than one (it
// may be omitted when the site has exactly one blind spot). Note is the context
// (required — an annotation with no note is drift). By records authorship for
// audit (a human handle, or an agent id/model); it must be drawn only from this
// committed config — never wall-clock or a live session id — so output stays
// deterministic.
type Annotation struct {
	Site string `yaml:"site"`
	Kind string `yaml:"kind,omitempty"`
	Note string `yaml:"note"`
	By   string `yaml:"by,omitempty"`
	// Claim is an OPTIONAL structured assertion of the boundary effect behind the
	// seam, written as the canonical corpus effect key ("PUBLISH email.sent",
	// "db DELETE ledger"). It is a falsifiable, machine-checkable form of the note:
	// the impeach lens grades it CONFIRMED when the corpus observed that exact effect
	// severed at the site, UNCONFIRMED when the corpus witnessed the seam but not this
	// effect (a sample's silence is never proof of absence — never "false"), and
	// UNWITNESSED when no corpus reaches the site. It stays disclosure-only: even a
	// CONFIRMED claim never closes the blind spot or feeds a verdict. No format
	// validation here — an unmatched claim simply reads UNCONFIRMED (fail-closed
	// disclosure), and config stays decoupled from the impeach key space.
	Claim string `yaml:"claim,omitempty"`
}

// ResolveAnnotationKind binds one annotation to a single blind-spot kind given the
// DISTINCT kinds detected at its site. It is the single source of truth for the
// annotation→blind-spot binding rule, shared by the producer-side merge (graphio,
// which embeds the bound annotation in the graph) and the read-only MCP `annotate`
// proposer (which validates a proposed annotation against the live manifest). One
// rule in one place means the proposer can never suggest an annotation the build
// would reject, or vice versa — parity is guarded by a test on each side.
//
// Fail closed: a site with no detected blind spot is an orphan (a stale FQN or
// moved code); an empty requestedKind binds a site that has exactly one kind but is
// ambiguous on a multi-kind site; a named kind absent at a live site yields a typed
// *KindAbsentError. That last case is the only tolerable one: a caller may downgrade
// it to a warn-and-skip when the requested kind is algorithm-fragile (§22), so it is
// typed distinctly from the orphan/ambiguity errors a caller must always fail on. The
// returned error names the site and the kinds actually present, so a caller (or an
// agent) can correct or classify the annotation without another round-trip.
func ResolveAnnotationKind(site, requestedKind string, kindsAtSite []string) (string, error) {
	distinct := sortedUnique(kindsAtSite)
	if len(distinct) == 0 {
		return "", fmt.Errorf("no blind spot detected at site %q — a stale annotation or moved code", site)
	}
	if requestedKind == "" {
		if len(distinct) != 1 {
			return "", fmt.Errorf("site %q carries %d blind-spot kinds (%s); set kind to disambiguate", site, len(distinct), strings.Join(distinct, ", "))
		}
		return distinct[0], nil
	}
	for _, k := range distinct {
		if k == requestedKind {
			return requestedKind, nil
		}
	}
	return "", &KindAbsentError{Site: site, RequestedKind: requestedKind, Present: distinct}
}

// KindAbsentError is returned by ResolveAnnotationKind when the requested kind is
// not present at a site that DOES carry other blind spots — distinct from an orphan
// (no blind spot at the site at all, a stale FQN or moved code). The site is live,
// so a present-but-different kind is a (site, kind) SKEW: when the requested kind is
// algorithm-fragile (blindspots.AlgoFragile — its presence flips with --algo), the
// producer-side merge warns and skips the disclosure-only annotation rather than
// failing the build (§22), while a non-fragile mismatch still fails closed. config
// cannot import blindspots, so the fragility test lives in the callers; this typed
// error is the single signal they branch on. Present names the kinds actually at the
// site (sorted, deterministic) so a caller can correct or classify without a
// round-trip; Error() is byte-identical to the prior inline message.
type KindAbsentError struct {
	Site          string
	RequestedKind string
	Present       []string
}

func (e *KindAbsentError) Error() string {
	return fmt.Sprintf("no %q blind spot at site %q (present: %s)", e.RequestedKind, e.Site, strings.Join(e.Present, ", "))
}

// sortedUnique returns the distinct values of ss in lexical order — a
// run-independent ordering so an ambiguity/absence error message is deterministic.
func sortedUnique(ss []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// DeclaredBlindSpot is one ratified seam (plan §8). Site is the FQN to blind (the
// severance Site the impeachment localized). Kind defaults to "ImpeachmentSeam" (the
// behaviorally-discovered category) and must, when set, name a recognized
// blindspots.Kind. Reason records the impeachment witness; it is required for audit
// (a seam blinded without a stated reason is drift, not a ratified disclosure).
type DeclaredBlindSpot struct {
	Site   string `yaml:"site"`
	Kind   string `yaml:"kind,omitempty"`
	Reason string `yaml:"reason,omitempty"`
}

// RouterHint declares a per-method HTTP router for root discovery. Each named
// function registers a handler for the HTTP method that is its name uppercased
// (chi's "Get" and echo's "GET" both map to GET), with the route at RouteArg and
// the handler func at HandlerArg (logical positions, excluding any receiver).
type RouterHint struct {
	Package    string   `yaml:"package"`    // import path declaring the router type
	Methods    []string `yaml:"methods"`    // registration function names, e.g. [GET, POST]
	RouteArg   *int     `yaml:"routeArg"`   // logical position of the route string; nil => 0
	HandlerArg *int     `yaml:"handlerArg"` // logical position of the handler func; nil => 1
}

// defaultHighFanOutThreshold is the built-in fan-out flag threshold.
const defaultHighFanOutThreshold = 8

// FanOutThreshold returns the configured high-fan-out threshold, or the default.
func (c *Config) FanOutThreshold() int {
	if c.Static.HighFanOutThreshold > 0 {
		return c.Static.HighFanOutThreshold
	}
	return defaultHighFanOutThreshold
}

// CanonConfig holds the policies the behavioral canonicalizer applies (canon
// spec §4). Every field defaults, so an empty document yields the spec's
// recommended defaults: retain tiers 1–2, the OTel-semconv attribute allowlist,
// and the built-in redaction matchers.
type CanonConfig struct {
	// SalienceTier is the minimum tier retained in the snapshot: "warn" keeps
	// tiers 1–2 (default), "info" keeps 1–3, "debug"/"all" keep everything. Spans
	// above the threshold are dropped and their survivors promoted.
	SalienceTier string `yaml:"salienceTier"`
	// AttributeAllowlist names extra attribute keys to retain in the snapshot.
	// By default the canonicalizer folds identity-bearing attributes into the op
	// key and keeps only a normalized db.statement, so the snapshot stays small;
	// listing a key here keeps it (after redaction) alongside the op.
	AttributeAllowlist []string `yaml:"attributeAllowlist"`
	// RedactKeys names retained attribute keys whose values are always replaced
	// with a type placeholder, on top of the built-in UUID / numeric-id /
	// timestamp value matchers. Only attributes that are retained (an allowlisted
	// key) are projected, so a redact key has effect only when it is also kept.
	RedactKeys []string `yaml:"redactKeys"`
	// OrderGuardMs is the out-of-process sibling-ordering guard, in milliseconds.
	// Two disjoint sibling spans separated by more than this are ordered
	// sequentially (a reliable happens-before); a smaller gap leaves them
	// unordered rather than over-claiming a sequence. 0 => default (100ms), well
	// past same-host scheduling jitter; raise it in a high-latency environment.
	OrderGuardMs int `yaml:"orderGuardMs"`
	// MessagingShortHexIDs opts into templating short (8–15 char) hex id tokens
	// embedded in messaging destination labels (eb-dev-evt-fddd7c99-v1 ->
	// eb-dev-evt-{id}-v1), on top of the always-on UUID / numeric / long-hex
	// templating. Off by default: a short hex token is ambiguous with a stable name
	// segment, so this is a deliberate opt-in for instrumentation whose topic/queue
	// names bake first-party ids shorter than a UUID — keeping the default safe for
	// adopters whose names happen to contain hex-ish but stable segments.
	MessagingShortHexIDs bool `yaml:"messagingShortHexIDs"`
}

// ClassifyHints name the libraries flowmap cannot infer: loggers, the bus client,
// the DB layer, and the outbound HTTP/RPC seam (tier-map spec §3a). Each entry is
// an import path, optionally narrowed to one symbol with "#Name" (e.g.
// "example.com/svc/internal/eventbus#Publish"); a bare path matches any call into
// that package.
type ClassifyHints struct {
	Telemetry  []string `yaml:"telemetry"`
	BusPublish []string `yaml:"busPublish"`
	BusConsume []string `yaml:"busConsume"`
	DB         []string `yaml:"db"`
	// HTTP names outbound HTTP/RPC seam functions. By convention the call's first
	// three string arguments are the peer, method, and route template, read by the
	// boundary extractor to name the external dependency.
	HTTP []string `yaml:"http"`
	// ObjectStore, Cache, and RPC name the "method-named outbound" effect kinds: an
	// object-store/blob SDK (kind `blob`), a cache client (kind `cache`), and a
	// non-HTTP RPC peer (kind `rpc`). Unlike HTTP they carry no readable
	// peer/method/route triple, so the operation is the callee method name and their
	// write-ness is NOT inferred from it (a method-name heuristic could be silently
	// wrong) — it is disclosed as a budget-unenforceable effect, the same fail-closed
	// treatment as a non-constant DB call.
	ObjectStore []string `yaml:"objectStore"`
	Cache       []string `yaml:"cache"`
	RPC         []string `yaml:"rpc"`
}

// EntrypointHints declares the entry points root discovery cannot reach by
// call-resolution — author-asserted roots, the same trust model as static.routers
// (the author declares an HTTP router), extended to two classes the registrar
// resolver provably cannot follow:
//
//   - Callbacks: a handler registered into a library whose DISPATCH is
//     out-of-module (an inbox consume handler, an outbox Publisher/FailureHandler).
//     The registration call is in scope but the handler value threads through a
//     struct field / constructor / closure — a value-flow chain root discovery
//     cannot follow, because it runs BEFORE the call graph is built (no points-to
//     to lean on). Such callbacks are otherwise ORPHANED (zero in-edges), so their
//     effect cones — DB writes, cloud provisioning — vanish from the reachable
//     graph. This is a coverage gap, not cosmetic.
//   - Workers: a `go`-launched background worker (a reconcile loop). These ARE
//     reachable via wiring from main, so it is not a soundness hole — but they are
//     not entry points, so their write surface is attributed to main rather than to
//     a worker route and cannot be gated per-worker. Declaring one roots it as its
//     own entry so its surface is attributable.
//
// Each entry is a fully-qualified "import/path#Symbol" reference, like an
// obligation ref (both halves required); a symbol naming several methods (same
// name, different receivers) roots ALL matches — over-approximate and sound, since
// a declared root only ever turns provenAbsent → reachable. A declared root is
// ASSERTED, not discovered, so it is disclosed AS a declared callback/worker
// entrypoint, never laundered into a discovered route.
//
// This MUST stay separate from classify.busConsume: busConsume entries are
// edge-classification (PUBLISH/CONSUME) registrar hints, not handler declarations.
// Direct-rooting a busConsume hint mis-roots it — one edge-classification hint
// expands to several spurious "consume" entrypoints, one per interface impl.
//
// Completeness is the author's job once declared: the mechanism roots what is
// declared, it does not discover the set. A missed declaration is a silent gap,
// same as today; a stale one (a reference matching no function) is disclosed as a
// blind spot in root discovery (drift surfaced, not dropped).
type EntrypointHints struct {
	Callbacks []string `yaml:"callbacks,omitempty"`
	Workers   []string `yaml:"workers,omitempty"`
}

// Rule maps a feature pattern to a tier. An empty MatchSpec matches everything.
type Rule struct {
	Name  string    `yaml:"name,omitempty"`
	Match MatchSpec `yaml:"match"`
	Tier  int       `yaml:"tier"`
}

// MatchSpec constrains normalized features. Empty string / nil fields are
// unconstrained. Identity is an identity glob ('*' crosses separators).
type MatchSpec struct {
	Boundary   string `yaml:"boundary,omitempty"`
	Effect     string `yaml:"effect,omitempty"`
	Origin     string `yaml:"origin,omitempty"`
	Fallible   *bool  `yaml:"fallible,omitempty"`
	Concurrent *bool  `yaml:"concurrent,omitempty"`
	Identity   string `yaml:"identity,omitempty"`
}

// Pin forces a tier for symbols/events matching an identity glob, ahead of all
// rules.
type Pin struct {
	Identity string `yaml:"identity"`
	Tier     int    `yaml:"tier"`
}

// LoadDir reads and validates the FileName document in dir. It is the single
// config-loading path both the static and behavioral pipelines use, so they
// always agree on a service's tier rules, pins, and canon knobs. A missing file
// is not an error — it yields the zero Config (defaults apply); an unreadable or
// malformed file is a hard error.
func LoadDir(dir string) (*Config, error) {
	path := filepath.Join(dir, FileName)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("flowmap config: read %s: %w", path, err)
	}
	return Load(b)
}

// Discover walks up from startDir for a FileName document and returns the
// directory holding it. The search is bounded to the enclosing module: it stops
// at the first directory containing a go.mod (the module root), so a stray
// .flowmap.yaml in a parent module, the repository root, or a developer's home
// directory is never picked up. This mirrors how a per-service config lives at
// its service module root. Returns ok=false when no in-module config exists.
func Discover(startDir string) (dir string, ok bool) {
	d := startDir
	for {
		if fileExists(filepath.Join(d, FileName)) {
			return d, true
		}
		// The module root bounds the search: the config is a per-service document
		// at or below its module root, never above it.
		if fileExists(filepath.Join(d, "go.mod")) {
			return "", false
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false
		}
		d = parent
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Load parses and validates a .flowmap.yaml document. Unknown keys are rejected.
// An empty document is valid and yields the zero config (all defaults).
func Load(b []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("flowmap config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.Static.HighFanOutThreshold < 0 {
		return fmt.Errorf("flowmap config: static.highFanOutThreshold %d must be >= 0", c.Static.HighFanOutThreshold)
	}
	if c.CatchAll < 0 || c.CatchAll > 4 {
		return fmt.Errorf("flowmap config: catchAll %d out of range 0..4", c.CatchAll)
	}
	for i, r := range c.Rules {
		if r.Tier < 1 || r.Tier > 4 {
			return fmt.Errorf("flowmap config: rules[%d].tier %d out of range 1..4", i, r.Tier)
		}
	}
	for i, p := range c.Pins {
		if p.Tier < 1 || p.Tier > 4 {
			return fmt.Errorf("flowmap config: pins[%d].tier %d out of range 1..4", i, p.Tier)
		}
	}
	if _, ok := salienceTiers[c.Canon.SalienceTier]; c.Canon.SalienceTier != "" && !ok {
		return fmt.Errorf("flowmap config: canon.salienceTier %q not one of warn|info|debug|all", c.Canon.SalienceTier)
	}
	names := make(map[string]bool, len(c.Obligations))
	for i, r := range c.Obligations {
		if r.Name == "" {
			return fmt.Errorf("flowmap config: obligations[%d]: name is required", i)
		}
		if names[r.Name] {
			return fmt.Errorf("flowmap config: obligations[%d]: duplicate name %q", i, r.Name)
		}
		names[r.Name] = true
		release := r.Acquire != "" || len(r.Release) > 0
		precede := r.Require != "" || r.Before != ""
		if release == precede {
			return fmt.Errorf("flowmap config: obligations[%d] (%s): exactly one of acquire/release or require/before is required", i, r.Name)
		}
		if release {
			if r.Acquire == "" || len(r.Release) == 0 {
				return fmt.Errorf("flowmap config: obligations[%d] (%s): acquire and at least one release are both required", i, r.Name)
			}
			if r.FromCallers {
				return fmt.Errorf("flowmap config: obligations[%d] (%s): fromCallers applies only to require/before rules", i, r.Name)
			}
			for _, ref := range append([]string{r.Acquire}, r.Release...) {
				if err := validRef(ref); err != nil {
					return fmt.Errorf("flowmap config: obligations[%d] (%s): %w", i, r.Name, err)
				}
			}
		} else {
			if r.Require == "" || r.Before == "" {
				return fmt.Errorf("flowmap config: obligations[%d] (%s): require and before are both required", i, r.Name)
			}
			for _, ref := range []string{r.Require, r.Before} {
				if err := validRef(ref); err != nil {
					return fmt.Errorf("flowmap config: obligations[%d] (%s): %w", i, r.Name, err)
				}
			}
		}
	}
	for i, r := range c.Static.Routers {
		if r.Package == "" {
			return fmt.Errorf("flowmap config: static.routers[%d].package is required", i)
		}
		if len(r.Methods) == 0 {
			return fmt.Errorf("flowmap config: static.routers[%d] (%s) lists no methods", i, r.Package)
		}
		if r.RouteArg != nil && *r.RouteArg < 0 {
			return fmt.Errorf("flowmap config: static.routers[%d].routeArg %d must be >= 0", i, *r.RouteArg)
		}
		if r.HandlerArg != nil && *r.HandlerArg < 0 {
			return fmt.Errorf("flowmap config: static.routers[%d].handlerArg %d must be >= 0", i, *r.HandlerArg)
		}
	}
	// A ratified seam must name a site to blind and a reason (the impeachment
	// witness). A seam with no site blinds nothing; a seam with no reason is
	// undisclosed drift, not a ratified disclosure — fail closed at load rather
	// than silently merging a reasonless blind spot. The Kind is validated against
	// the recognized set in graphio (config cannot import blindspots — that package
	// imports config), where the merge happens.
	for i, b := range c.Static.DeclaredBlindSpots {
		if b.Site == "" {
			return fmt.Errorf("flowmap config: static.declaredBlindSpots[%d]: site is required", i)
		}
		if b.Reason == "" {
			return fmt.Errorf("flowmap config: static.declaredBlindSpots[%d] (%s): reason is required (the impeachment witness)", i, b.Site)
		}
	}
	// An empty exempt prefix would match every package — silently suppressing the
	// whole ExternalBoundaryCall disclosure. Fail closed at load rather than blind
	// the surface by typo.
	for i, p := range c.Static.ExternalBoundaryExempt {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("flowmap config: static.externalBoundaryExempt[%d] is empty; an empty prefix would suppress every external boundary", i)
		}
	}
	// An annotation must name the site it decorates and carry a note (the context).
	// The (site, kind) match against a detected blind spot — and the kind's
	// recognition — are checked in graphio where the merge sees the manifest (config
	// cannot import blindspots). A site/note-less annotation is drift, refused here.
	for i, a := range c.Static.Annotations {
		if a.Site == "" {
			return fmt.Errorf("flowmap config: static.annotations[%d]: site is required", i)
		}
		if strings.TrimSpace(a.Note) == "" {
			return fmt.Errorf("flowmap config: static.annotations[%d] (%s): note is required (the context)", i, a.Site)
		}
	}
	// A declared entrypoint anchors on a specific function, so — like an obligation
	// ref — it must name both an import path and a symbol. A well-formed reference
	// that matches no function at analysis time is a different failure (runtime
	// drift), disclosed there as a blind spot; this catches the typo at load.
	for i, ref := range c.Entrypoints.Callbacks {
		if err := validRef(ref); err != nil {
			return fmt.Errorf("flowmap config: entrypoints.callbacks[%d]: %w", i, err)
		}
	}
	for i, ref := range c.Entrypoints.Workers {
		if err := validRef(ref); err != nil {
			return fmt.Errorf("flowmap config: entrypoints.workers[%d]: %w", i, err)
		}
	}
	return nil
}

// validRef checks an obligation function reference: "import/path#Symbol" with
// both halves non-empty. Unlike a classify hint, a bare package is rejected —
// an obligation anchors on a specific function.
func validRef(s string) error {
	i := strings.IndexByte(s, '#')
	if i <= 0 || i == len(s)-1 {
		return fmt.Errorf("reference %q must have the form import/path#Symbol", s)
	}
	return nil
}

// OrderGuard returns the out-of-process sibling-ordering guard, or the 100ms
// default. A disjoint sibling gap larger than this is treated as a reliable
// happens-before; a smaller gap is left unordered.
func (c *CanonConfig) OrderGuard() time.Duration {
	if c.OrderGuardMs > 0 {
		return time.Duration(c.OrderGuardMs) * time.Millisecond
	}
	return 100 * time.Millisecond
}

// salienceTiers maps a salience name to the maximum tier retained in a snapshot.
var salienceTiers = map[string]int{"warn": 2, "info": 3, "debug": 4, "all": 4}

// SalienceThreshold is the maximum (least consequential) tier kept in the
// canonical snapshot; spans with a higher tier number are dropped and promoted.
// Absent in config => "warn" => 2 (canon spec §4).
func (c *Config) SalienceThreshold() int {
	if t, ok := salienceTiers[c.Canon.SalienceTier]; ok {
		return t
	}
	return 2
}

// UsesDefaults reports whether the built-in tier rules layer beneath user rules.
// Absent in config => true.
func (c *Config) UsesDefaults() bool { return c.UseDefaults == nil || *c.UseDefaults }

// CatchAllTier is the tier for operations that match no rule (default 3).
func (c *Config) CatchAllTier() int {
	if c.CatchAll == 0 {
		return 3
	}
	return c.CatchAll
}
