// Package ir defines flowmap's authoritative canonical intermediate
// representation: the deterministic, run-independent shape of one exercised flow.
// It is the gated golden file's on-disk form and the single type shared by the
// canonicalizer (which produces it) and the renderer, diff, and golden lifecycle
// (which read it).
//
// This package is part of flowmap's PUBLIC API. Target service repositories
// depend on it (transitively, through the flow and harness packages) when they
// author flow tests, so its exported shape is a stable contract.
package ir

import (
	"encoding/json"

	"github.com/jyang234/golang-code-graph/internal/canonjson"
)

// Kind mirrors the OTel span kind of an operation.
type Kind string

const (
	KindServer   Kind = "server"
	KindClient   Kind = "client"
	KindInternal Kind = "internal"
	KindProducer Kind = "producer"
	KindConsumer Kind = "consumer"
)

// SchemaVersion identifies the canonical trace's on-disk form. A flowmap change
// to that form bumps it; because the version participates in snapshot equality, a
// bump makes every committed golden mismatch and must be cleared by a coordinated
// regeneration (`go test -update`) — the real blast radius, made explicit rather
// than silent (plan [H6]).
const SchemaVersion = "flowmap.trace/v1"

// CanonicalTrace is the deterministic representation of one exercised flow.
// Equality of two traces (ignoring Discards) is the snapshot assertion.
type CanonicalTrace struct {
	Flow          string          `json:"flow"`
	Service       string          `json:"service"`
	SchemaVersion string          `json:"schema_version"`
	Root          *CanonicalSpan  `json:"root"`
	Discards      DiscardManifest `json:"discards"`
}

// CanonicalSpan is one normalized operation in the flow tree.
type CanonicalSpan struct {
	Op   string `json:"op"`
	Kind Kind   `json:"kind"`
	Peer string `json:"peer"`
	// Service is the owning service (OTel resource service.name) for this
	// operation. It is empty for an in-process single-service capture (the trace's
	// Service is the one lifeline) and populated for an out-of-process whole-flow
	// capture that crosses services, so the renderer can place each operation on
	// its owning lifeline. Omitted when empty, so single-service goldens are
	// unaffected.
	Service   string `json:"service,omitempty"`
	Tier      int    `json:"tier"`
	Status    string `json:"status,omitempty"`
	ErrorType string `json:"errorType,omitempty"`
	// Async marks an operation reached across a broker by an OTLP span link
	// (FOLLOWS_FROM) rather than an in-process call edge — a separately-polled
	// continuation caused by, not synchronously invoked during, its parent. The
	// renderer draws the hop into it as a distinct asynchronous interaction.
	// Omitted when false, so synchronous in-process goldens are unaffected.
	Async    bool              `json:"async,omitempty"`
	Attrs    map[string]string `json:"attrs,omitempty"`
	Children []ChildGroup      `json:"children,omitempty"`
}

// ChildGroup makes ordering semantics explicit. Groups are emitted in
// happens-before order; within a Concurrent group, members are stored in
// canonical-key order so a race does not perturb the snapshot.
type ChildGroup struct {
	Concurrent bool `json:"concurrent,omitempty"`
	// Unordered marks members whose relative order could not be reliably
	// established (out-of-process siblings disjoint within the order guard, or
	// untimed) — distinct from Concurrent, which asserts parallelism. It claims
	// neither a sequence nor a race.
	Unordered    bool             `json:"unordered,omitempty"`
	Multiplicity string           `json:"multiplicity,omitempty"`
	Members      []*CanonicalSpan `json:"members"`
}

// DiscardManifest records, for review transparency, which dimensions were dropped
// during canonicalization. It carries only deterministic markers — never volatile
// counts — and is excluded from snapshot equality.
type DiscardManifest struct {
	IDs        string   `json:"ids,omitempty"`        // e.g. "dropped"
	Timing     string   `json:"timing,omitempty"`     // e.g. "dropped"
	Redactions []string `json:"redactions,omitempty"` // sorted attribute keys redacted
	Loops      []string `json:"loops,omitempty"`      // sorted ops collapsed by multiplicity
}

// Marshal renders the trace as canonical, deterministic JSON — the golden file's
// bytes.
func (t *CanonicalTrace) Marshal() ([]byte, error) {
	return canonjson.Marshal(t)
}

// Load parses canonical JSON produced by Marshal back into a trace.
func Load(b []byte) (*CanonicalTrace, error) {
	var t CanonicalTrace
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	return &t, nil
}
