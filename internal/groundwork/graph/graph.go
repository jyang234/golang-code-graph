// Package graph loads and indexes the call graph that flowmap emits (`flowmap
// graph <service>`) and is the substrate every groundwork surface is built on.
//
// groundwork deliberately declares its own value types here rather than import
// flowmap's internal graphio: the graph JSON is the *interface* between the two
// programs (flowmap produces it, groundwork consumes it), and keeping a separate,
// explicit decode of that interface is what lets the two sit in different trust
// domains — flowmap runs in trusted CI, groundwork only ever reads the file it is
// handed. The shapes are kept in lockstep with graphio by the committed goldens
// under testdata/groundwork and the regen script beside them.
package graph

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// boundaryPrefix marks an edge target that is a typed external sink (a DB op, an
// outbound call, a bus publish/consume) rather than a first-party function. Such
// targets never appear in Nodes; they are the leaves of the effect surface.
const boundaryPrefix = "boundary:"

// dynamicMarker is flowmap's token for a boundary effect whose target could not
// be named statically (e.g. a publish to a topic chosen at runtime). An edge
// whose target contains it is a known hole in the graph's knowledge — the
// frontier where reachability stops being sound.
const dynamicMarker = "<dynamic>"

// Graph is one call-graph view as emitted by `flowmap graph`. It is the whole,
// unscoped service graph unless Entrypoint is set.
type Graph struct {
	// Stamp is the producer's caller-supplied identity (typically the deployed
	// commit SHA). Consumers pass --expect to verify they hold the graph for
	// the code they think they do — a stale map mis-triages. Opt-in at both
	// ends: an absent stamp is only an error when verification was asked for.
	Stamp      string `json:"stamp,omitempty"`
	Entrypoint string `json:"entrypoint,omitempty"`

	// Algo is the call-graph construction algorithm flowmap built this graph on
	// (rta|vta|cha) and Caveats are its recorded soundness/precision notes —
	// provenance, not gate inputs. fitness/review/verify echo them so a verdict
	// self-certifies its substrate. All three algorithms are sound over-
	// approximations modulo the reflection/unsafe frontier already carried in
	// BlindSpots, so a proof (must_not_reach PROVEN-ABSENT) is valid on any of
	// them; recording which one ran is for auditability. Absent on graphs from a
	// pre-provenance flowmap — an empty Algo means "unrecorded", never "unsound".
	Algo    string   `json:"algo,omitempty"`
	Caveats []string `json:"caveats,omitempty"`

	Nodes       []Node            `json:"nodes"`
	Edges       []Edge            `json:"edges"`
	BlindSpots  []BlindSpot       `json:"blind_spots"`
	Obligations []Obligation      `json:"obligations,omitempty"`
	EffectOrder []EffectOrderFact `json:"effect_order,omitempty"`
	Entrypoints []Entrypoint      `json:"entrypoints,omitempty"`
}

// Entrypoint is one named root flowmap discovered: an HTTP route or a consumed
// topic, joined to its handler function. Names are registration-site literals
// (a stdlib root may lack a method; a mounted route carries only its leaf
// pattern), so consumers match them segment-wise, never exactly-or-nothing.
type Entrypoint struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Fn   string `json:"fn"`
}

// EffectOrderFact is one partial-effect order fact flowmap computed from a
// function's CFG: the named committed effect can execute before the named
// fallible call on some path (Always: on every path reaching it). Triage reads
// these to answer "if this call faults, what may already be committed?" —
// possibly-committed when Always is false, certainly-committed when true.
type EffectOrderFact struct {
	Fn         string `json:"fn"`
	Effect     string `json:"effect"`
	EffectSite string `json:"effect_site"`
	Callee     string `json:"callee"`
	CalleeSite string `json:"callee_site"`
	Always     bool   `json:"always,omitempty"`
	// Via names the ALWAYS-effect callee when the effect is committed inside
	// a call one frame down (a derived site, CX-3) — provenance for the
	// fault card, never part of fact identity.
	Via string `json:"via,omitempty"`
}

// Obligation is one path-obligation verdict flowmap computed from a function's
// SSA CFG against a .flowmap.yaml rule. groundwork only judges it: VIOLATED is
// a gate-failing finding, CANT-PROVE and UNMATCHED are disclosed abstentions,
// SATISFIED is the proof and produces no finding. Identity is (rule, fn, site);
// detail is presentation only.
//
// Status is an open vocabulary across the trust boundary: flowmap and
// groundwork decode this section independently and on purpose, so the judge
// MUST fail closed on a status it does not recognize (surface a caution,
// never fall through) — the convention for every graph-carried enum.
type Obligation struct {
	Rule   string `json:"rule"`
	Kind   string `json:"kind"`
	Fn     string `json:"fn,omitempty"`
	Site   string `json:"site,omitempty"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Node is one first-party function.
type Node struct {
	FQN      string `json:"fqn"`
	Sig      string `json:"sig"`
	Tier     int    `json:"tier"`
	Fallible bool   `json:"fallible,omitempty"`
}

// Edge is a call from a first-party function (From, always a Node) to another
// first-party function or to a typed boundary sink (To). Boundary names the
// external-effect kind for boundary edges (outbound-sync, outbound-async,
// inbound); it is empty for internal function-to-function edges.
type Edge struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Tier       int    `json:"tier"`
	Boundary   string `json:"boundary,omitempty"`
	Concurrent bool   `json:"concurrent,omitempty"`
}

// BlindSpot is one disclosed gap in the graph's knowledge. Site is a first-party
// FQN (reflect, HighFanOut) or a package path (unsafe, cgo, go:linkname). The
// graph view carries only the graph-completeness subset; the boundary subset
// (dynamically-named publish/dispatch) rides the boundary contract instead, and
// surfaces in the graph as a <dynamic> edge target.
type BlindSpot struct {
	Kind   string `json:"kind"`
	Site   string `json:"site"`
	Detail string `json:"detail"`
}

// ProvenanceLine renders the one-line call-graph substrate disclosure shared by
// every groundwork surface that echoes provenance (fitness, review, verify), so
// they word it identically. An empty algo means the graph predates provenance
// recording — stated as "unrecorded", never implying a substrate. caveats are
// the recorded soundness/precision notes, joined.
func ProvenanceLine(algo string, caveats []string) string {
	if algo == "" {
		return "substrate: unrecorded (graph predates provenance; regenerate with current flowmap)\n"
	}
	line := "substrate: " + algo
	if len(caveats) > 0 {
		line += " — " + strings.Join(caveats, "; ")
	}
	return line + "\n"
}

// IsBoundary reports whether the edge targets an external sink rather than a
// first-party function.
func (e Edge) IsBoundary() bool { return strings.HasPrefix(e.To, boundaryPrefix) }

// IsDynamic reports whether the edge targets a boundary effect the graph could
// not name statically — a soundness frontier for any reachability claim through
// it.
func (e Edge) IsDynamic() bool { return strings.Contains(e.To, dynamicMarker) }

// Load decodes a graph from JSON. It rejects unknown fields so a flowmap schema
// change that groundwork has not been taught about fails loudly here rather than
// being silently dropped.
func Load(r io.Reader) (*Graph, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var g Graph
	if err := dec.Decode(&g); err != nil {
		return nil, fmt.Errorf("groundwork/graph: decode: %w", err)
	}
	if g.Nodes == nil {
		return nil, fmt.Errorf("groundwork/graph: missing nodes (not a flowmap graph?)")
	}
	return &g, nil
}

// LoadFile reads and decodes a graph from a file path.
func LoadFile(path string) (*Graph, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	g, err := Load(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return g, nil
}
