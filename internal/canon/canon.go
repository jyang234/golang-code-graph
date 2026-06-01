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
	"time"

	"github.com/jyang234/golang-code-graph/internal/canon/opkey"
	"github.com/jyang234/golang-code-graph/internal/canon/promote"
	sqlnorm "github.com/jyang234/golang-code-graph/internal/canon/sql"
	"github.com/jyang234/golang-code-graph/internal/canonjson"
	"github.com/jyang234/golang-code-graph/internal/capture"
	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/model"
	"github.com/jyang234/golang-code-graph/internal/tiermap"
	"github.com/jyang234/golang-code-graph/ir"
)

// defaultGuard is zero for in-process capture: span start/end come from one
// monotonic clock, so genuinely sequential operations always satisfy
// B.start ≥ A.end and only truly overlapping intervals are read as concurrent
// (canon §3.3). A positive guard would misread microsecond-adjacent sequential
// calls — which is most of an in-process flow — as a race. Cross-clock-domain
// capture (deferred) supplies its own guard.
const defaultGuard = 0

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
		guard:      defaultGuard,
		redactions: map[string]bool{},
		loops:      map[string]bool{},
		allow:      buildAllowSet(cfg),
		redactKeys: stringSet(cfg.Canon.RedactKeys),
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
		Flow:     cf.Flow,
		Service:  service,
		Root:     rootSpan,
		Discards: c.discards(),
	}
	return trace, nil
}

type canonicalizer struct {
	cfg        *config.Config
	classifier *tiermap.Classifier
	guard      time.Duration
	redactions map[string]bool
	loops      map[string]bool
	allow      map[string]bool
	redactKeys map[string]bool
}

// build turns one captured span and its subtree into a CanonicalSpan: it derives
// the op key and peer, projects and normalizes attributes, assigns the tier, and
// groups the children by happens-before order (recursing into each).
func (c *canonicalizer) build(s *capture.Span, childrenOf map[string][]*capture.Span) *ir.CanonicalSpan {
	op, peer := opkey.Of(s.Kind, s.Attrs, s.Name)
	cs := &ir.CanonicalSpan{
		Op:        op,
		Kind:      s.Kind,
		Peer:      peer,
		Status:    normalizeStatus(s.Status),
		ErrorType: s.ErrorType,
		Attrs:     c.projectAttrs(s),
	}
	cs.Tier, _ = c.classifier.Classify(c.features(s, op))
	cs.Children = c.group(childrenOf[s.ID], childrenOf)
	return cs
}

// group orders a parent's children into happens-before child groups and collapses
// data-dependent repetition. Siblings are clustered by caller-clock interval
// overlap: a maximal run of overlapping intervals is one race-ordered
// (Concurrent) group with members in canonical-key order; non-overlapping
// siblings fall into separate sequential groups ordered by start time. Because
// all siblings share one process clock here, interval comparison is reliable
// (canon §3.3).
func (c *canonicalizer) group(kids []*capture.Span, childrenOf map[string][]*capture.Span) []ir.ChildGroup {
	if len(kids) == 0 {
		return nil
	}
	ordered := make([]*capture.Span, len(kids))
	copy(ordered, kids)
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].Start.Equal(ordered[j].Start) {
			return ordered[i].Start.Before(ordered[j].Start)
		}
		return ordered[i].ID < ordered[j].ID
	})

	var groups []ir.ChildGroup
	cluster := []*capture.Span{ordered[0]}
	maxEnd := ordered[0].End
	flush := func() {
		members := make([]*ir.CanonicalSpan, 0, len(cluster))
		for _, s := range cluster {
			members = append(members, c.build(s, childrenOf))
		}
		concurrent := len(members) > 1
		if concurrent {
			sort.SliceStable(members, func(i, j int) bool { return members[i].Op < members[j].Op })
		}
		groups = append(groups, ir.ChildGroup{Concurrent: concurrent, Members: members})
	}
	for _, s := range ordered[1:] {
		if !s.Start.Before(maxEnd.Add(c.guard)) {
			// s starts after the cluster fully ended (beyond the guard) → sequential.
			flush()
			cluster = []*capture.Span{s}
			maxEnd = s.End
			continue
		}
		cluster = append(cluster, s)
		if s.End.After(maxEnd) {
			maxEnd = s.End
		}
	}
	flush()
	return c.collapseLoops(groups)
}

// collapseLoops folds data-dependent repetition into one representative with a
// multiplicity class so processing 3 vs. 300 items yields the same snapshot
// (canon §3.7). Two shapes are collapsed: a run of consecutive sequential groups
// with identical canonical subtrees, and identical members within a concurrent
// group.
func (c *canonicalizer) collapseLoops(groups []ir.ChildGroup) []ir.ChildGroup {
	// Dedupe identical concurrent members.
	for gi := range groups {
		g := &groups[gi]
		if g.Concurrent {
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
		if g.Concurrent || len(g.Members) != 1 {
			out = append(out, g)
			continue
		}
		sig := signature(g.Members[0])
		j := i + 1
		for j < len(groups) && !groups[j].Concurrent && len(groups[j].Members) == 1 && signature(groups[j].Members[0]) == sig {
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
func (c *canonicalizer) features(s *capture.Span, op string) model.Features {
	f := model.Features{Identity: op, Fallible: normalizeStatus(s.Status) == capture.StatusError}
	switch s.Kind {
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
func signature(s *ir.CanonicalSpan) string {
	b, _ := canonjson.Marshal(s)
	return string(b)
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
	switch op {
	case "SELECT", "select":
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
