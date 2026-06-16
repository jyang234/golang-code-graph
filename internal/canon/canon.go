// Package canon is the load-bearing behavioral transform: it turns a captured,
// scoped flow into flowmap's deterministic, run-independent IR (canon spec).
// Getting this right is what lets the snapshot gate produce a true diff instead
// of churn — two runs of the same flow over the same seeded data must yield
// byte-identical IR.
//
// Canonicalize runs the spec's ordered passes. It refuses an incomplete capture
// (the first line of defense against a silent false golden), assembles the span
// tree, derives canonical op keys, normalizes URLs and SQL, redacts volatile
// values, orders siblings from the caller's single clock domain (concurrent on
// any ambiguity), assigns salience tiers with the shared classifier, and
// contracts the tree — collapsing loops and promoting survivors of dropped
// sub-threshold nodes. Every dimension that varies between runs is discarded and
// recorded in the manifest, which is excluded from snapshot equality.
package canon

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jyang234/golang-code-graph/capture"
	"github.com/jyang234/golang-code-graph/internal/canon/opkey"
	"github.com/jyang234/golang-code-graph/internal/canon/promote"
	sqlnorm "github.com/jyang234/golang-code-graph/internal/canon/sql"
	"github.com/jyang234/golang-code-graph/internal/canonjson"
	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/model"
	"github.com/jyang234/golang-code-graph/internal/tiermap"
	"github.com/jyang234/golang-code-graph/ir"
)

// ErrIncomplete is returned when canonicalization is asked to snapshot a
// truncated capture. It is a hard stop, never a snapshot (harness §7, canon
// §3.1).
var ErrIncomplete = errors.New("canon: capture is incomplete; refusing to snapshot a truncated trace")

// Canonicalize transforms a captured flow into the canonical IR under cfg
// (nil => defaults). It returns ErrIncomplete if the capture is not complete.
func Canonicalize(cf capture.CapturedFlow, cfg *config.Config) (*ir.CanonicalTrace, error) {
	if !cf.Complete {
		return nil, ErrIncomplete
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	c := &canonicalizer{
		cfg:        cfg,
		classifier: tiermap.New(cfg),
		redactions: map[string]bool{},
		loops:      map[string]bool{},
		allow:      buildAllowSet(cfg),
		redactKeys: stringSet(cfg.Canon.RedactKeys),
		postHoc:    cf.Mode == capture.ModePostHoc,
	}

	byID := make(map[string]*capture.Span, len(cf.Spans))
	childrenOf := make(map[string][]*capture.Span, len(cf.Spans))
	for i := range cf.Spans {
		s := &cf.Spans[i]
		byID[s.ID] = s
	}
	for i := range cf.Spans {
		s := &cf.Spans[i]
		if _, ok := byID[s.ParentID]; ok {
			childrenOf[s.ParentID] = append(childrenOf[s.ParentID], s)
		}
	}

	root := cf.Root
	if root == nil {
		return nil, fmt.Errorf("canon: capture has no reconstructed root")
	}

	// 3.1 assembly: build the tree from the root; surface orphans rather than
	// dropping them (canon §3.1).
	rootSpan := c.build(root, childrenOf)
	if orphans := c.orphans(cf.Spans, byID, root, childrenOf); len(orphans) > 0 {
		rootSpan.Children = append(rootSpan.Children, orphans...)
	}

	// 3.7 structural normalization: salience filtering as tree contraction. The
	// root (the tier-1 entry) is never dropped.
	threshold := cfg.SalienceThreshold()
	promote.Filter(rootSpan, func(s *ir.CanonicalSpan) bool { return s.Tier <= threshold })

	service := cf.Service
	if service == "" {
		service = cfg.Service
	}
	trace := &ir.CanonicalTrace{
		Flow:          cf.Flow,
		Service:       service,
		SchemaVersion: ir.SchemaVersion,
		Root:          rootSpan,
		Discards:      c.discards(),
	}
	return trace, nil
}

type canonicalizer struct {
	cfg        *config.Config
	classifier *tiermap.Classifier
	redactions map[string]bool
	loops      map[string]bool
	allow      map[string]bool
	redactKeys map[string]bool
	// postHoc selects the out-of-process ordering profile (post-hoc design
	// [P10.3]), driven by the capture's Mode. Out of process there is no
	// goroutine dispatch signal and the exported caller-clock intervals are not a
	// run-independent ordering signal for siblings, so happens-before among
	// siblings cannot be reliably re-established. Per canon §3.3 rule 3
	// (ambiguous ⇒ concurrent), the profile groups a parent's children into a
	// single concurrent group ordered by canonical op key — timing- and
	// id-independent — instead of clustering by caller-clock overlap. Parent→child
	// nesting (the real happens-before that survives in OTLP) is untouched.
	postHoc bool
}

// build turns one captured span and its subtree into a CanonicalSpan: it derives
// the op key and peer, projects and normalizes attributes, assigns the tier, and
// groups the children by happens-before order (recursing into each).
func (c *canonicalizer) build(s *capture.Span, childrenOf map[string][]*capture.Span) *ir.CanonicalSpan {
	op, peer := opkey.Of(s.Kind, s.Attrs, s.Name, opkey.Options{ShortHexIDs: c.cfg.Canon.MessagingShortHexIDs})
	// A managed-broker call (AWS SDK SNS/SQS) arrives as a CLIENT span but is
	// behaviorally a producer/consumer; normalize to the messaging role so every
	// kind-keyed consumer (gate, syscontext, render, tiering) classifies it as the
	// broker interaction it is. For non-messaging spans this is the raw kind.
	kind := opkey.EffectiveKind(s.Kind, s.Attrs)
	cs := &ir.CanonicalSpan{
		Op:        op,
		Kind:      kind,
		Peer:      peer,
		Service:   s.Attr("service.name"), // owning lifeline; "" in-process (omitempty)
		Async:     s.AsyncLink,            // reached across a broker via a span link
		Status:    normalizeStatus(s.Status),
		ErrorType: s.ErrorType,
		Attrs:     c.projectAttrs(s),
	}
	cs.Tier, _ = c.classifier.Classify(c.features(kind, s, op))
	cs.Children = c.group(s.Goroutine, childrenOf[s.ID], childrenOf)
	return cs
}

// group orders a parent's children into happens-before child groups and collapses
// data-dependent repetition. Siblings are clustered into concurrency components:
// two siblings are joined when capture.Concurrent reports they raced — preferring
// the structural goroutine signal (parentGoroutine) and falling back to
// caller-clock interval overlap. Each component becomes a group, emitted in
// happens-before order by its earliest member; a multi-member (concurrent) group
// stores its members in canonical-key order so a race never perturbs the snapshot
// (canon §3.3).
func (c *canonicalizer) group(parentGoroutine uint64, kids []*capture.Span, childrenOf map[string][]*capture.Span) []ir.ChildGroup {
	if len(kids) == 0 {
		return nil
	}

	if c.postHoc {
		return c.groupPostHoc(kids, childrenOf)
	}

	ordered := make([]*capture.Span, len(kids))
	copy(ordered, kids)
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].Start.Equal(ordered[j].Start) {
			return ordered[i].Start.Before(ordered[j].Start)
		}
		return ordered[i].ID < ordered[j].ID
	})

	// Union siblings that ran concurrently into components.
	n := len(ordered)
	uf := make([]int, n)
	for i := range uf {
		uf[i] = i
	}
	find := func(x int) int {
		for uf[x] != x {
			uf[x] = uf[uf[x]]
			x = uf[x]
		}
		return x
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if capture.Concurrent(*ordered[i], *ordered[j], parentGoroutine) {
				if ri, rj := find(i), find(j); ri != rj {
					uf[ri] = rj
				}
			}
		}
	}

	// Collect components in start order (ordered is start-sorted, so a component's
	// smallest index is its earliest member; order components by that index).
	comps := map[int][]int{}
	var roots []int
	for i := 0; i < n; i++ {
		r := find(i)
		if _, seen := comps[r]; !seen {
			roots = append(roots, r)
		}
		comps[r] = append(comps[r], i)
	}
	sort.SliceStable(roots, func(i, j int) bool { return comps[roots[i]][0] < comps[roots[j]][0] })

	var groups []ir.ChildGroup
	for _, r := range roots {
		idxs := comps[r]
		members := make([]*ir.CanonicalSpan, 0, len(idxs))
		for _, idx := range idxs {
			members = append(members, c.build(ordered[idx], childrenOf))
		}
		concurrent := len(members) > 1
		if concurrent {
			// Same tie-break as the post-hoc path: op key then canonical subtree
			// signature, so two same-op concurrent siblings are ordered
			// run-independently and a race never perturbs the byte-identical IR.
			bySig(members)
		}
		groups = append(groups, ir.ChildGroup{Concurrent: concurrent, Members: members})
	}
	return c.collapseLoops(groups)
}

// groupPostHoc orders siblings out of process, where the goroutine dispatch
// signal is gone but the exported intervals remain. Absolute timestamps jitter
// run-to-run, but the *order* of disjoint intervals does not, so three states are
// distinguished instead of forcing everything concurrent (canon §3.3, post-hoc):
//
//   - overlapping intervals → concurrent (genuine parallelism, par).
//   - disjoint by more than the order guard → sequential (a reliable
//     happens-before; groups emitted in start order).
//   - disjoint within the guard, or untimed → unordered (a distinct render that
//     claims neither a sequence nor parallelism).
//
// Concurrent and unordered members are sorted by op key then canonical subtree
// signature so a same-op tie is run-independent.
func (c *canonicalizer) groupPostHoc(kids []*capture.Span, childrenOf map[string][]*capture.Span) []ir.ChildGroup {
	ordered := make([]*capture.Span, len(kids))
	copy(ordered, kids)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].Start.Equal(ordered[j].Start) {
			return ordered[i].Start.Before(ordered[j].Start)
		}
		return ordered[i].ID < ordered[j].ID
	})

	// Overlap components: two siblings whose caller-clock intervals intersect are
	// concurrent (goroutine signal absent out of process, so pass 0).
	n := len(ordered)
	uf := make([]int, n)
	for i := range uf {
		uf[i] = i
	}
	find := func(x int) int {
		for uf[x] != x {
			uf[x] = uf[uf[x]]
			x = uf[x]
		}
		return x
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if capture.Concurrent(*ordered[i], *ordered[j], 0) {
				if ri, rj := find(i), find(j); ri != rj {
					uf[ri] = rj
				}
			}
		}
	}

	type unit struct {
		members    []*ir.CanonicalSpan
		start, end time.Time
		timed      bool
	}
	comps := map[int][]int{}
	var roots []int
	for i := 0; i < n; i++ {
		r := find(i)
		if _, seen := comps[r]; !seen {
			roots = append(roots, r)
		}
		comps[r] = append(comps[r], i)
	}
	sort.SliceStable(roots, func(i, j int) bool { return comps[roots[i]][0] < comps[roots[j]][0] })

	units := make([]unit, 0, len(roots))
	for _, r := range roots {
		u := unit{timed: true}
		for k, idx := range comps[r] {
			s := ordered[idx]
			u.members = append(u.members, c.build(s, childrenOf))
			if s.Start.IsZero() || s.End.IsZero() {
				u.timed = false
			}
			if k == 0 || s.Start.Before(u.start) {
				u.start = s.Start
			}
			if k == 0 || s.End.After(u.end) {
				u.end = s.End
			}
		}
		if len(u.members) > 1 {
			bySig(u.members) // members of a concurrent component
		}
		units = append(units, u)
	}

	if len(units) == 1 {
		u := units[0]
		return c.collapseLoops([]ir.ChildGroup{{Concurrent: len(u.members) > 1, Members: u.members}})
	}

	// Partition the start-sorted units into runs separated by a RELIABLE gap (both
	// neighbors timed and disjoint by more than the order guard). A reliable gap is
	// a run-stable happens-before boundary and becomes a sequence boundary; units
	// that fall within the guard of a neighbor (or are untimed) cannot be ordered
	// relative to each other and stay together as one unordered group. This refines
	// the former all-or-nothing rule — under which a single ambiguous gap collapsed
	// an otherwise strictly-sequential sibling set into one wall-of-par block: now
	// only the genuinely-ambiguous neighbors group, and every cleanly-separated
	// effect renders as its own ordered step.
	guard := c.cfg.Canon.OrderGuard()
	reliableGap := func(prev, cur unit) bool {
		return prev.timed && cur.timed && cur.start.Sub(prev.end) > guard
	}
	var groups []ir.ChildGroup
	for i := 0; i < len(units); {
		j := i + 1
		for j < len(units) && !reliableGap(units[j-1], units[j]) {
			j++
		}
		block := units[i:j]
		if len(block) == 1 {
			u := block[0]
			groups = append(groups, ir.ChildGroup{Concurrent: len(u.members) > 1, Members: u.members})
		} else {
			// A run of mutually-unorderable units: present together as unordered
			// rather than over-claim a sequence (op-key/arbitrary) or parallelism.
			var all []*ir.CanonicalSpan
			for _, u := range block {
				all = append(all, u.members...)
			}
			bySig(all)
			groups = append(groups, ir.ChildGroup{Unordered: len(all) > 1, Members: all})
		}
		i = j
	}
	return c.collapseLoops(groups)
}

// collapseLoops folds data-dependent repetition into one representative with a
// multiplicity class so processing 3 vs. 300 items yields the same snapshot
// (canon §3.7). Two shapes are collapsed: a run of consecutive sequential groups
// with identical canonical subtrees, and identical members within a concurrent or
// unordered group.
func (c *canonicalizer) collapseLoops(groups []ir.ChildGroup) []ir.ChildGroup {
	// Dedupe identical concurrent/unordered members.
	for gi := range groups {
		g := &groups[gi]
		if g.Concurrent || g.Unordered {
			deduped := g.Members[:0:0]
			seen := map[string]bool{}
			collapsed := false
			for _, m := range g.Members {
				sig := signature(m)
				if seen[sig] {
					collapsed = true
					c.loops[m.Op] = true
					continue
				}
				seen[sig] = true
				deduped = append(deduped, m)
			}
			if collapsed {
				g.Multiplicity = "1..*"
				g.Members = deduped
			}
		}
	}
	// Collapse runs of identical consecutive sequential groups.
	var out []ir.ChildGroup
	for i := 0; i < len(groups); i++ {
		g := groups[i]
		// Only truly sequential single-member groups form a happens-before run.
		// An Unordered group satisfies !Concurrent but claims no sequence, so
		// folding it here would mislabel distinct unordered effects as a loop.
		if g.Concurrent || g.Unordered || len(g.Members) != 1 {
			out = append(out, g)
			continue
		}
		sig := signature(g.Members[0])
		j := i + 1
		for j < len(groups) && !groups[j].Concurrent && !groups[j].Unordered && len(groups[j].Members) == 1 && signature(groups[j].Members[0]) == sig {
			j++
		}
		if j-i > 1 {
			g.Multiplicity = "1..*"
			c.loops[g.Members[0].Op] = true
			i = j - 1
		}
		out = append(out, g)
	}
	return out
}

// features derives the normalized feature vector for tier classification from the
// span's kind, op, and attributes (canon §3.6). It mirrors the static extractor's
// intent so a publish is tier 1 and an internal compute is tier 3 whether seen
// statically or at runtime.
func (c *canonicalizer) features(kind ir.Kind, s *capture.Span, op string) model.Features {
	f := model.Features{Identity: op, Fallible: normalizeStatus(s.Status) == capture.StatusError}
	switch kind {
	case ir.KindServer, ir.KindConsumer:
		f.Boundary, f.Effect, f.Origin = model.BoundaryInbound, model.EffectIO, model.OriginFirstParty
	case ir.KindProducer:
		f.Boundary, f.Effect, f.Origin = model.BoundaryOutboundAsync, model.EffectMutate, model.OriginFirstParty
	case ir.KindClient:
		f.Boundary, f.Origin = model.BoundaryOutboundSync, model.OriginThirdParty
		if dbOp := dbOperation(s.Attrs); dbOp != "" {
			f.Effect = dbEffect(dbOp)
		} else {
			f.Effect = model.EffectIO
		}
	default: // internal
		// An internal-kind span carrying db attributes is a DB operation —
		// opkey.Of keys it as one, so it must tier as one (ext-read / mutate),
		// not as ordinary compute, or it would be mis-tiered and dropped. Some
		// instrumentations (notably ORMs) open DB spans as internal rather than
		// client.
		if dbOp := dbOperation(s.Attrs); dbOp != "" {
			f.Boundary, f.Effect, f.Origin = model.BoundaryOutboundSync, dbEffect(dbOp), model.OriginThirdParty
		} else {
			f.Boundary, f.Effect, f.Origin = model.BoundaryInternal, model.EffectCompute, model.OriginFirstParty
		}
	}
	return f
}

// projectAttrs keeps only the salient detail worth showing alongside the op key.
// SQL statements are normalized; configured allowlist keys are kept and redacted.
// Most identity-bearing attributes are folded into the op key already, so the
// retained set stays small and reviewable (matching the spec's worked example,
// where only a normalized db.statement survives).
func (c *canonicalizer) projectAttrs(s *capture.Span) map[string]string {
	if s.Attrs == nil {
		return nil
	}
	out := map[string]string{}
	if stmt := opkey.Statement(s.Attrs); stmt != "" {
		out["db.statement"] = sqlnorm.Normalize(stmt).Statement
	}
	for k := range c.allow {
		if v, ok := s.Attrs[k]; ok {
			out[k] = c.redact(k, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// redact replaces a volatile value with a type placeholder, recording the key so
// the manifest can disclose that a value was dropped without exposing it.
func (c *canonicalizer) redact(key, val string) string {
	if c.redactKeys[key] {
		c.redactions[key] = true
		return "<redacted>"
	}
	if p, ok := placeholder(val); ok {
		c.redactions[key] = true
		return p
	}
	return val
}

// orphans gathers spans whose parent is missing from the scoped set (a scoping or
// completeness problem) and attaches them as trailing sequential groups so they
// are surfaced, never silently dropped (canon §3.1).
func (c *canonicalizer) orphans(spans []capture.Span, byID map[string]*capture.Span, root *capture.Span, childrenOf map[string][]*capture.Span) []ir.ChildGroup {
	var extra []ir.ChildGroup
	var heads []*capture.Span
	for i := range spans {
		s := &spans[i]
		if s.ID == root.ID {
			continue
		}
		if _, ok := byID[s.ParentID]; !ok {
			heads = append(heads, s)
		}
	}
	sort.Slice(heads, func(i, j int) bool {
		if !heads[i].Start.Equal(heads[j].Start) {
			return heads[i].Start.Before(heads[j].Start)
		}
		return heads[i].ID < heads[j].ID
	})
	for _, h := range heads {
		extra = append(extra, ir.ChildGroup{Members: []*ir.CanonicalSpan{c.build(h, childrenOf)}})
	}
	return extra
}

// discards builds the manifest of dropped dimensions: ids and timing are always
// discarded; redactions and loops list the affected keys/ops in sorted order. It
// carries deterministic markers only — never volatile counts — and is excluded
// from snapshot equality.
func (c *canonicalizer) discards() ir.DiscardManifest {
	return ir.DiscardManifest{
		IDs:        "dropped",
		Timing:     "dropped",
		Redactions: sortedKeys(c.redactions),
		Loops:      sortedKeys(c.loops),
	}
}

// signature renders a span subtree to its canonical bytes for equality testing
// (loop detection). It excludes nothing volatile because the tree is already
// normalized at this point.
// signature is the canonical subtree key used to order same-op concurrent and
// unordered siblings run-independently (bySig) and to fold identical adjacent
// groups into loops. canonjson.Marshal is deterministic and only fails on a value
// it cannot represent (a channel/func/complex field); a CanonicalSpan never
// carries one, so a failure here means the IR is corrupt. Fail closed by panicking
// rather than returning "": a swallowed error would collapse EVERY signature to the
// empty string, silently degrading the tie-break to op-only and letting a race
// perturb the "canonical" output — the confidently-wrong, nondeterministic result
// this package exists to prevent, which is strictly worse than a crash.
func signature(s *ir.CanonicalSpan) string {
	b, err := canonjson.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("canon: signature marshal failed on a span this package assumes is always representable (corrupt IR): %v", err))
	}
	return string(b)
}

// bySig orders members by op key then canonical subtree signature. It is the
// single ordering both the in-process and post-hoc paths apply to concurrent /
// unordered siblings: a same-op tie is broken by subtree content, never by
// run-dependent start time, so a race cannot perturb the byte-identical IR.
func bySig(ms []*ir.CanonicalSpan) {
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].Op != ms[j].Op {
			return ms[i].Op < ms[j].Op
		}
		return signature(ms[i]) < signature(ms[j])
	})
}

func normalizeStatus(s string) string {
	switch s {
	case capture.StatusOK, capture.StatusError:
		return s
	default:
		return ""
	}
}

func dbOperation(attrs map[string]string) string {
	if attrs == nil {
		return ""
	}
	if op := attrs["db.operation"]; op != "" {
		return op
	}
	if op := attrs["db.operation.name"]; op != "" {
		return op
	}
	if stmt := opkey.Statement(attrs); stmt != "" {
		return sqlnorm.Normalize(stmt).Operation
	}
	return ""
}

func dbEffect(op string) model.Effect {
	// db.operation arrives in arbitrary case (raw OTel attribute), while the
	// SQL-normalize path yields upper-case. Classify case-insensitively so a
	// read is never mis-tiered as a mutation on casing alone — two captures of
	// the same query differing only in case must produce identical IR.
	switch strings.ToUpper(strings.TrimSpace(op)) {
	case "SELECT":
		return model.EffectRead
	default:
		return model.EffectMutate
	}
}

func buildAllowSet(cfg *config.Config) map[string]bool {
	return stringSet(cfg.Canon.AttributeAllowlist)
}

func stringSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
