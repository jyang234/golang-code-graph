package impeach

import (
	"sort"
	"strconv"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/ir"
)

// The span↔node map (plan §7): a TOTAL function from an observed span to
// {node FQN} ∪ {⊥}, where ⊥ is an honest "this span does not anchor" that the
// severance walk (§6) absorbs as a gap. Precision is a dial (the L0/L1/L2 harness
// levels); SOUNDNESS is invariant — the map never GUESSES a node, so a coarser
// level yields a coarser-but-sound Site, never a wrong one.
//
// Three span classes anchor differently: an ENTRY span via the route→entrypoint
// join (mapEntry, severance.go), an EFFECT span via its canonical label→emitter
// (staticEmitters, severance.go), and the hard INTERNAL span via its L1
// `flowmap.fqn` runtime tag reconciled through canonFQN — handled here.

// FQNTagKey is the L1 capture tag (§7): the runtime FQN of the function that
// emitted a span, set by the capture harness build-time/in-process. It is absent
// from today's pipeline (§14-D: the trace model carries no FQN tags yet), so an
// untagged corpus simply maps no internal spans and the walk stays at L0 (entry +
// effect anchors only) — the tag is a precision dial, never a correctness premise.
const FQNTagKey = "flowmap.fqn"

// Internal-span map outcomes (§7). Only `mapped` is an anchor; the rest are
// honest non-anchors. `absent-from-graph` is special: a tag that parses to a valid
// FQNKey the graph has NO node for is a DIRECTLY localized missing node — sharper
// than the walk — but sound only when canonFQN's ⊥ is symmetric, so it is trusted
// at L2 (the tag IS the ssa string) and a WEAK HINT at L1 until the symmetry fuzz
// proves it (§7 fail-closed property 1, §12.5).
const (
	MapMapped          = "mapped"
	MapAbsentFromGraph = "absent-from-graph"
	MapUntagged        = "untagged"
	MapAmbiguous       = "ambiguous"
)

// nodeIndex is the reverse index canonFQN ⇒ node FQNs, built once over the graph's
// first-party nodes. A key that resolves to MORE THAN ONE node is a COLLISION: two
// node spellings the key cannot tell apart, so any span keying to it is `ambiguous`
// ⇒ ⊥, never one of the two guessed (§7 fail-closed property 2). Nodes whose own
// spelling is ⊥ (closures, generics) are absent from the index by construction, so
// they can never be a mapped target.
type nodeIndex struct {
	byKey map[FQNKey][]string
}

// buildNodeIndex indexes ix's first-party nodes by their canonFQN key, each
// bucket sorted so a collision's recorded members are deterministic. Pure function
// of the graph.
func buildNodeIndex(ix *graph.Index) *nodeIndex {
	nx := &nodeIndex{byKey: make(map[FQNKey][]string)}
	for _, fqn := range ix.Nodes() {
		if k, ok := canonFQN(fqn); ok {
			nx.byKey[k] = append(nx.byKey[k], fqn)
		}
	}
	for k := range nx.byKey {
		sort.Strings(nx.byKey[k])
	}
	return nx
}

// spanAnchor is the outcome of mapping one internal span: the anchored Node (only
// when Outcome == MapMapped), the parsed tag (carried on MapAbsentFromGraph as the
// sharp missing-node identity), the Outcome, and a human Reason for the ⊥ classes.
type spanAnchor struct {
	Node    string
	Tag     string
	Outcome string
	Reason  string
}

// mapInternal maps one internal span through its L1 FQN tag (§7). It never
// guesses: no tag ⇒ untagged (⊥), a ⊥ tag ⇒ untagged with the canonFQN reason, a
// key with no node ⇒ absent-from-graph (the sharp signal), exactly one node ⇒
// mapped, more than one ⇒ ambiguous (⊥).
func (nx *nodeIndex) mapInternal(s *ir.CanonicalSpan) spanAnchor {
	tag := ""
	if s != nil {
		tag = s.Attrs[FQNTagKey]
	}
	if tag == "" {
		return spanAnchor{Outcome: MapUntagged, Reason: "no " + FQNTagKey + " tag"}
	}
	k, ok := canonFQN(tag)
	if !ok {
		return spanAnchor{Outcome: MapUntagged, Reason: "tag ⊥ (" + fqnBotReason(tag) + ")"}
	}
	switch nodes := nx.byKey[k]; len(nodes) {
	case 0:
		return spanAnchor{Tag: tag, Outcome: MapAbsentFromGraph, Reason: "tag keys a function the graph has no node for"}
	case 1:
		return spanAnchor{Node: nodes[0], Outcome: MapMapped}
	default:
		return spanAnchor{Outcome: MapAmbiguous, Reason: "tag key collides with " + strconv.Itoa(len(nodes)) + " nodes"}
	}
}
