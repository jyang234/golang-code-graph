// Package diff computes the structural, prioritized change set between two
// canonical traces — the observed flow versus its committed golden
// (golden-diff spec §3, §4). It diffs the IR tree, not the JSON text: a moved
// subtree is one Reordered entry, not a delete-plus-add cascade, because
// canonicalization already gave every node a stable identity (its Op).
//
// Changes are prioritized by reusing the tier-map's intent a third time:
// contract changes (a published/consumed event or external dependency
// added/removed) outrank tier-1 changes (status, mutations), which outrank
// structural changes (reorders, concurrency, cardinality), which outrank
// lower-tier attribute edits. The reviewer sees the headline first.
package diff

import (
	"sort"
	"strconv"
	"strings"

	"github.com/jyang234/golang-code-graph/ir"
)

// Type is a taxonomy member (golden-diff spec §3).
type Type string

const (
	Added              Type = "Added"
	Removed            Type = "Removed"
	Changed            Type = "Changed"
	Reordered          Type = "Reordered"
	ConcurrencyChanged Type = "ConcurrencyChanged"
	CardinalityChanged Type = "CardinalityChanged"
)

// Priority ranks a change for the reviewer; lower sorts first.
type Priority int

const (
	PriorityContract   Priority = iota // bus/world interface changed — the headline
	PriorityTier1                      // status (ok→error), mutations
	PriorityStructural                 // reorders, concurrency, cardinality
	PriorityLower                      // attribute edits, lower-tier add/remove
)

// Change is one typed, prioritized difference.
type Change struct {
	Type     Type
	Priority Priority
	Op       string // the affected operation's canonical key
	Detail   string // human-readable, copy-pasteable
}

// String renders a change as a prioritized, prefixed line, e.g.
// "[CONTRACT] ADDED GET fraud-svc /check/{id}" or "[T1] payment-gw …: status ok→error".
func (c Change) String() string { return c.prefix() + " " + c.Detail }

func (c Change) prefix() string {
	switch c.Type {
	case Reordered:
		return "[REORDER]"
	case ConcurrencyChanged:
		return "[CONCURRENCY]"
	case CardinalityChanged:
		return "[CARDINALITY]"
	}
	switch c.Priority {
	case PriorityContract:
		return "[CONTRACT]"
	case PriorityTier1:
		return "[T1]"
	default:
		return "[MINOR]"
	}
}

// Diff returns the prioritized change set transforming a (the golden) into b (the
// observed). An empty result means the traces are identical. A nil trace cannot
// be compared and yields no changes.
func Diff(a, b *ir.CanonicalTrace) []Change {
	d := &differ{}
	if a == nil || b == nil {
		return nil
	}
	// A schema-version change means flowmap's canonical form moved under the
	// golden; it is not a behavioral change but it requires a coordinated
	// regeneration, so surface it as the headline.
	if a.SchemaVersion != b.SchemaVersion {
		d.add(Changed, PriorityContract, "",
			"schema version "+orNone(a.SchemaVersion)+"→"+orNone(b.SchemaVersion)+" (canonical form changed; regenerate goldens)")
	}
	// Flow and service identity are part of the trace; a change here means a
	// different flow or self-lifeline, which the per-flow golden should surface.
	if a.Flow != b.Flow {
		d.add(Changed, PriorityTier1, "", "flow "+orNone(a.Flow)+"→"+orNone(b.Flow))
	}
	if a.Service != b.Service {
		d.add(Changed, PriorityTier1, "", "service "+orNone(a.Service)+"→"+orNone(b.Service))
	}
	// Reconstruct the root relationship explicitly: a missing root on one side is
	// a whole-flow add/remove, not "no change".
	switch {
	case a.Root == nil && b.Root == nil:
		// nothing to compare below the trace level
	case a.Root == nil:
		d.add(Added, nodePriority(b.Root), b.Root.Op, "ADDED "+human(b.Root.Op))
	case b.Root == nil:
		d.add(Removed, nodePriority(a.Root), a.Root.Op, "REMOVED "+human(a.Root.Op))
	default:
		// Root ↔ root is a forced match (golden-diff spec §3).
		d.matchedPair(a.Root, b.Root)
	}
	d.sortStable()
	return d.changes
}

type differ struct {
	changes []Change
}

func (d *differ) add(t Type, p Priority, op, detail string) {
	d.changes = append(d.changes, Change{Type: t, Priority: p, Op: op, Detail: detail})
}

// matchedPair compares two nodes already matched by Op, then diffs their children.
func (d *differ) matchedPair(old, new *ir.CanonicalSpan) {
	d.compareAttrs(old, new)
	d.diffChildren(old, new)
}

// compareAttrs reports the per-node attribute differences (golden-diff spec §3).
// Status and error-class changes are tier-1; tier/peer/kind and salient attrs are
// ranked low so they never outrank a contract or tier-1 change.
func (d *differ) compareAttrs(old, new *ir.CanonicalSpan) {
	op := new.Op
	// Only the forced root match can have differing Ops; every other pair was
	// matched by equal Op. A changed entry op means the flow's identity changed.
	if old.Op != new.Op {
		d.add(Changed, PriorityTier1, op, "entry "+human(old.Op)+"→"+human(new.Op))
	}
	if old.Status != new.Status {
		d.add(Changed, PriorityTier1, op, human(op)+": status "+orUnset(old.Status)+"→"+orUnset(new.Status))
	}
	if old.ErrorType != new.ErrorType {
		d.add(Changed, PriorityTier1, op, human(op)+": error "+orNone(old.ErrorType)+"→"+orNone(new.ErrorType))
	}
	if old.Tier != new.Tier {
		// A reclassification *into* tier 1 means the operation just became
		// consequential (a newly tier-1 surface), which golden-diff spec §4 ranks
		// second only to contract; surface it as tier-1 rather than burying it.
		// Other tier shifts (demotions, escalations among lower tiers) stay low.
		p := PriorityLower
		if new.Tier == 1 && old.Tier != 1 {
			p = PriorityTier1
		}
		d.add(Changed, p, op, human(op)+": tier "+strconv.Itoa(old.Tier)+"→"+strconv.Itoa(new.Tier))
	}
	if old.Peer != new.Peer {
		d.add(Changed, PriorityLower, op, human(op)+": peer "+orNone(old.Peer)+"→"+orNone(new.Peer))
	}
	if old.Kind != new.Kind {
		d.add(Changed, PriorityLower, op, human(op)+": kind "+string(old.Kind)+"→"+string(new.Kind))
	}
	for _, k := range changedAttrKeys(old.Attrs, new.Attrs) {
		d.add(Changed, PriorityLower, op, human(op)+": "+k+" changed")
	}
}

// diffChildren matches a parent's children by Op (duplicates disambiguated by
// order), reports added/removed/reordered/concurrency/cardinality, and recurses.
func (d *differ) diffChildren(old, new *ir.CanonicalSpan) {
	oldSlots := flatten(old.Children)
	newSlots := flatten(new.Children)

	pairs, added, removed := matchSlots(oldSlots, newSlots)

	for _, s := range removed {
		d.add(Removed, nodePriority(s.span), s.span.Op, "REMOVED "+human(s.span.Op))
	}
	for _, s := range added {
		d.add(Added, nodePriority(s.span), s.span.Op, "ADDED "+human(s.span.Op))
	}

	// Reorders: only sequential members carry behavioral happens-before order, so
	// only they participate in reorder detection. Concurrent-group members are
	// stored in canonical-key order, not run order (ir.ChildGroup) — including
	// them would report a spurious reorder whenever a group's concurrency changes
	// or a concurrent sibling's canonical position shifts; that transition is
	// already reported as ConcurrencyChanged. Among the sequential matched pairs
	// (in old order), members outside the longest increasing subsequence of new
	// positions are the minimal moved set.
	var seqIdx []int
	var seqOrder []int
	for i, p := range pairs {
		if p.old.concurrent || p.new.concurrent {
			continue
		}
		seqIdx = append(seqIdx, i)
		seqOrder = append(seqOrder, p.new.order)
	}
	kept := lisKept(seqOrder)
	for k, i := range seqIdx {
		if !kept[k] {
			d.add(Reordered, PriorityStructural, pairs[i].new.span.Op, human(pairs[i].new.span.Op)+" reordered")
		}
	}

	// Per matched pair: group-tag changes, then recurse.
	for _, p := range pairs {
		if p.old.concurrent != p.new.concurrent {
			d.add(ConcurrencyChanged, PriorityStructural, p.new.span.Op,
				human(p.new.span.Op)+": now "+concurrencyWord(p.new.concurrent))
		}
		if p.old.multiplicity != p.new.multiplicity {
			d.add(CardinalityChanged, PriorityStructural, p.new.span.Op,
				human(p.new.span.Op)+": multiplicity "+orOne(p.old.multiplicity)+"→"+orOne(p.new.multiplicity))
		}
		d.matchedPair(p.old.span, p.new.span)
	}
}

// sortStable orders changes by priority. It is a stable sort, so within a
// priority the changes keep their discovery (tree) order — the headline contract
// and tier-1 deltas precede structural and minor ones, ties unshuffled.
func (d *differ) sortStable() {
	sort.SliceStable(d.changes, func(i, j int) bool {
		return d.changes[i].Priority < d.changes[j].Priority
	})
}

// slot is one child flattened out of its group, carrying the group's concurrency
// and multiplicity plus its position for reorder detection.
type slot struct {
	span         *ir.CanonicalSpan
	concurrent   bool
	multiplicity string
	order        int
}

func flatten(groups []ir.ChildGroup) []slot {
	var out []slot
	i := 0
	for _, g := range groups {
		for _, m := range g.Members {
			out = append(out, slot{span: m, concurrent: g.Concurrent, multiplicity: g.Multiplicity, order: i})
			i++
		}
	}
	return out
}

type pair struct{ old, new slot }

// matchSlots pairs old and new children by Op, disambiguating same-Op duplicates
// by order (golden-diff spec §3, §7 default). Unmatched old → removed, unmatched
// new → added.
func matchSlots(oldSlots, newSlots []slot) (pairs []pair, added, removed []slot) {
	byOp := map[string][]int{}
	for i, s := range newSlots {
		byOp[s.span.Op] = append(byOp[s.span.Op], i)
	}
	matchedNew := make([]bool, len(newSlots))
	for _, os := range oldSlots {
		q := byOp[os.span.Op]
		if len(q) > 0 {
			ni := q[0]
			byOp[os.span.Op] = q[1:]
			matchedNew[ni] = true
			pairs = append(pairs, pair{old: os, new: newSlots[ni]})
		} else {
			removed = append(removed, os)
		}
	}
	for i, s := range newSlots {
		if !matchedNew[i] {
			added = append(added, s)
		}
	}
	return pairs, added, removed
}

// lisKept returns the indices of seq that belong to a longest increasing
// subsequence — the elements that did NOT move. The complement is the minimal
// reordered set.
func lisKept(seq []int) map[int]bool {
	n := len(seq)
	kept := make(map[int]bool, n)
	if n == 0 {
		return kept
	}
	dp := make([]int, n)
	prev := make([]int, n)
	best := 0
	for i := 0; i < n; i++ {
		dp[i], prev[i] = 1, -1
		for j := 0; j < i; j++ {
			if seq[j] < seq[i] && dp[j]+1 > dp[i] {
				dp[i], prev[i] = dp[j]+1, j
			}
		}
		if dp[i] > dp[best] {
			best = i
		}
	}
	for i := best; i != -1; i = prev[i] {
		kept[i] = true
	}
	return kept
}

// nodePriority classifies an added/removed node: a publish, a consume, or an
// outbound call to an external service is a contract change; an otherwise tier-1
// node is tier-1; everything else is low.
func nodePriority(s *ir.CanonicalSpan) Priority {
	if isContract(s) {
		return PriorityContract
	}
	if s.Tier == 1 {
		return PriorityTier1
	}
	return PriorityLower
}

// isContract reports whether a node sits on the inter-service boundary: a
// published or consumed event, or an outbound call to an external service. The
// database is excluded — it is the service's own store, not an inter-service
// surface (golden-diff spec §4, static-extractor spec §4, scope-enforcement §2).
// A DB client span and an external HTTP/RPC client span both carry a non-empty
// Peer (db.system vs peer.service), so they are indistinguishable by Peer alone;
// the canonical Op prefix is the reliable discriminator ("DB …" vs "HTTP …" /
// "RPC …").
func isContract(s *ir.CanonicalSpan) bool {
	switch s.Kind {
	case ir.KindProducer, ir.KindConsumer:
		return true
	case ir.KindClient:
		return s.Peer != "" && !isDBOp(s.Op)
	default:
		return false
	}
}

// isDBOp reports whether an op is a database operation ("DB <system> <op>
// <table>"), which the canonical-key step keys with a "DB " prefix.
func isDBOp(op string) bool { return strings.HasPrefix(op, "DB ") }

// changedAttrKeys returns the sorted keys whose values differ (added, removed, or
// modified) between two attribute maps.
func changedAttrKeys(a, b map[string]string) []string {
	var keys []string
	seen := map[string]bool{}
	for k, av := range a {
		seen[k] = true
		if bv, ok := b[k]; !ok || bv != av {
			keys = append(keys, k)
		}
	}
	for k, bv := range b {
		if seen[k] {
			continue
		}
		if av, ok := a[k]; !ok || av != bv {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// human strips the protocol prefix from an op for a readable change line, so
// "HTTP GET fraud-svc /check/{id}" reads as "GET fraud-svc /check/{id}".
func human(op string) string { return strings.TrimPrefix(op, "HTTP ") }

func concurrencyWord(concurrent bool) string {
	if concurrent {
		return "concurrent"
	}
	return "sequential"
}

func orUnset(s string) string { return orDefault(s, "unset") }
func orNone(s string) string  { return orDefault(s, "none") }
func orOne(s string) string   { return orDefault(s, "1") }

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
