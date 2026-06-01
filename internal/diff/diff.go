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
// observed). An empty result means the flows are structurally identical.
func Diff(a, b *ir.CanonicalTrace) []Change {
	d := &differ{}
	if a == nil || b == nil || a.Root == nil || b.Root == nil {
		return nil
	}
	// Root ↔ root is a forced match (golden-diff spec §3).
	d.matchedPair(a.Root, b.Root)
	d.sortStable()
	return d.changes
}

type differ struct {
	changes []Change
	seq     int // discovery order, for stable secondary sort
}

func (d *differ) add(t Type, p Priority, op, detail string) {
	d.changes = append(d.changes, Change{Type: t, Priority: p, Op: op, Detail: detail})
	d.seq++
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
		d.add(Changed, PriorityTier1, op, "entry "+old.Op+"→"+new.Op)
	}
	if old.Status != new.Status {
		d.add(Changed, PriorityTier1, op, human(op)+": status "+orUnset(old.Status)+"→"+orUnset(new.Status))
	}
	if old.ErrorType != new.ErrorType {
		d.add(Changed, PriorityTier1, op, human(op)+": error "+orNone(old.ErrorType)+"→"+orNone(new.ErrorType))
	}
	if old.Tier != new.Tier {
		d.add(Changed, PriorityLower, op, human(op)+": tier "+itoa(old.Tier)+"→"+itoa(new.Tier))
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

	// Reorders: among matched pairs (in old order), members outside the longest
	// increasing subsequence of new positions are the minimal moved set.
	newOrder := make([]int, len(pairs))
	for i, p := range pairs {
		newOrder[i] = p.new.order
	}
	kept := lisKept(newOrder)
	for i, p := range pairs {
		if !kept[i] {
			d.add(Reordered, PriorityStructural, p.new.span.Op, human(p.new.span.Op)+" reordered")
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

// sortStable orders changes by priority, then by discovery (tree) order, so the
// headline contract and tier-1 deltas precede structural and minor ones.
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
// outbound call to a peer is a contract change; an otherwise tier-1 node is
// tier-1; everything else is low.
func nodePriority(s *ir.CanonicalSpan) Priority {
	if isContract(s) {
		return PriorityContract
	}
	if s.Tier == 1 {
		return PriorityTier1
	}
	return PriorityLower
}

func isContract(s *ir.CanonicalSpan) bool {
	switch s.Kind {
	case ir.KindProducer, ir.KindConsumer:
		return true
	case ir.KindClient:
		return s.Peer != "" // an external dependency
	default:
		return false
	}
}

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

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
