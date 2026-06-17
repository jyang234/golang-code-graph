package fitness

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// ExceptionStatus is one active suppression and whether it still suppresses
// anything. Allow-lists accumulate; unaudited, the framework's honesty
// migrates into an unreviewed exception graveyard. A DEAD entry matches
// nothing in the current graph — the violation it once excused is gone, and
// the entry should be deleted before it silently excuses something new.
type ExceptionStatus struct {
	Source string `json:"source"` // "layering", "must_pass_through:<rule>", "blind_spot_ratchet"
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Site   string `json:"site,omitempty"`
	Reason string `json:"reason,omitempty"`
	Dead   bool   `json:"dead,omitempty"`
}

// Exceptions audits every allow-list in the policy against one graph.
//
// Liveness is decided by suppressed-set attribution: run Check twice — once as
// configured (baseline) and once with every audited allow-list emptied — and
// take the findings that appear only in the stripped run. Those are exactly
// what the allow-lists are suppressing, by finding Key. An entry is LIVE iff
// it matches at least one suppressed finding, using the same matcher its
// check uses (a layering entry via exempted; a pass-through entry via
// rule.Allowed on the bypass pair, scoped to its rule's findings by the
// rule-name summary prefix — names are validated unique). A blind-spot
// exception is live while the graph still carries a matching spot.
//
// This is identity-based on purpose: an earlier draft compared finding COUNTS
// per removed entry, and a live entry whose removal swaps a blind-frontier
// Caution for a bypass Violation kept the count equal — a gate-protecting
// exception reported DEAD. Sets cannot be fooled that way, and two Check runs
// replace N+1. Deterministic and read-only; it informs review, it does not
// gate.
func Exceptions(p *policy.Policy, ix *graph.Index) []ExceptionStatus {
	baseline := map[string]bool{}
	for _, f := range Check(p, ix).Findings {
		baseline[f.Key()] = true
	}
	var suppressed []Finding
	for _, f := range Check(stripAllows(p), ix).Findings {
		if !baseline[f.Key()] {
			suppressed = append(suppressed, f)
		}
	}

	var out []ExceptionStatus
	if p.Layering != nil {
		for _, a := range p.Layering.Allow {
			live := false
			for _, f := range suppressed {
				if f.Rule == "layering" && exempted([]policy.Exception{a}, f.From, f.To) {
					live = true
					break
				}
			}
			out = append(out, ExceptionStatus{
				Source: "layering", From: a.From, To: a.To, Reason: a.Reason, Dead: !live,
			})
		}
	}

	for ri := range p.MustPassThrough {
		rule := &p.MustPassThrough[ri]
		prefix := rule.Name + ": "
		for _, a := range rule.Allow {
			one := policy.PassRule{Allow: []policy.Exception{a}}
			live := false
			for _, f := range suppressed {
				if f.Rule == "must_pass_through" && strings.HasPrefix(f.Summary, prefix) && one.Allowed(f.From, f.To) {
					live = true
					break
				}
			}
			out = append(out, ExceptionStatus{
				Source: "must_pass_through:" + rule.Name, From: a.From, To: a.To, Reason: a.Reason, Dead: !live,
			})
		}
	}

	if p.BlindSpotRatchet != nil {
		for _, a := range p.BlindSpotRatchet.Allow {
			live := false
			for _, b := range ix.BlindSpots() {
				one := &policy.BlindSpotRatchet{Allow: []policy.BlindSpotException{a}}
				if one.Allows(b.Kind, b.Site) {
					live = true
					break
				}
			}
			out = append(out, ExceptionStatus{
				Source: "blind_spot_ratchet", Kind: a.Kind, Site: a.Site, Reason: a.Reason,
				Dead: !live,
			})
		}
	}

	// An effect-ratchet entry is live while the graph still carries a matching
	// write label — same present-fact test as a blind-spot exception (the
	// ratchet's DRIFT findings — new write targets — live in review, not fitness,
	// so suppressed-set attribution does not apply; the graph-independent
	// ratchet_coupling caution fitness does emit is a config disclosure, not an
	// allow-entry, so it is irrelevant here).
	if p.EffectRatchet != nil {
		var writeLabels []string
		for _, e := range ix.Edges() {
			if label, ok := WriteLabel(e); ok {
				writeLabels = append(writeLabels, label)
			}
		}
		for _, a := range p.EffectRatchet.Allow {
			one := &policy.EffectRatchet{Allow: []policy.EffectException{a}}
			live := false
			for _, l := range writeLabels {
				if one.Allows(l) {
					live = true
					break
				}
			}
			out = append(out, ExceptionStatus{
				Source: "effect_ratchet", Site: a.Target, Reason: a.Reason, Dead: !live,
			})
		}
	}

	// Total order: ratchet entries carry empty From/To, so Site, Kind and Reason
	// are the only distinguishers. Without Kind/Reason in the key, two
	// blind_spot_ratchet entries at one Site differing only in Kind tie on every
	// key and the unstable sort.Slice reorders them run-to-run.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		if a.Site != b.Site {
			return a.Site < b.Site
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Reason < b.Reason
	})
	return out
}

// stripAllows returns a copy of p with every audited allow-list emptied, so
// one Check run exposes everything the allow-lists collectively suppress.
// Blind-spot exceptions are not part of fitness findings and keep their own
// present-spot liveness test.
func stripAllows(p *policy.Policy) *policy.Policy {
	stripped := *p
	if p.Layering != nil {
		lay := *p.Layering
		lay.Allow = nil
		stripped.Layering = &lay
	}
	if len(p.MustPassThrough) > 0 {
		rules := append([]policy.PassRule{}, p.MustPassThrough...)
		for i := range rules {
			rules[i].Allow = nil
		}
		stripped.MustPassThrough = rules
	}
	return &stripped
}

// DeadCount tallies the dead entries — the number the audit wants at zero.
func DeadCount(xs []ExceptionStatus) int {
	n := 0
	for _, x := range xs {
		if x.Dead {
			n++
		}
	}
	return n
}

// String renders one entry for the audit listing.
func (x ExceptionStatus) String() string {
	state := "LIVE"
	if x.Dead {
		state = "DEAD"
	}
	what := x.Site
	if x.From != "" || x.To != "" {
		what = x.From + " → " + x.To
	}
	if x.Kind != "" {
		what = x.Kind + " " + what
	}
	s := fmt.Sprintf("[%s] %s — %s", state, x.Source, what)
	if x.Reason != "" {
		s += " (" + x.Reason + ")"
	}
	return s
}
