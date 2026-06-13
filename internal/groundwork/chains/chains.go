// Package chains composes the per-service facts groundwork already holds into a
// cross-service happens-before "chain card" (CX-5). It joins a publisher to its
// consumers by event name — exactly as the fleet-events lens does — and, for
// each join, renders a chain whose every link is labeled either **proven** (a
// fact computed from one service's graph: a publish's commit ordering, a
// consumer handler's effects and obligations) or **assumed** (the broker
// guarantee, declared in policy and never inferred — D-CX5).
//
// The surface is observational, non-gating: it answers "if this publish faulted,
// what was already committed, and what does the consumer do with the event?"
// without asserting a rule. A gating `chain` rule kind is a deliberate later
// step (E-CX5), not part of this package.
//
// It is honest about the fleet it was handed: an event published with no
// consumer loaded (or consumed with no producer loaded) is an OPEN chain, said
// so, not hidden; events a service cannot name statically (<dynamic> publishes)
// are disclosed as a frontier, never silently dropped.
package chains

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
)

// Link labels: a per-service graph fact vs. a declared broker guarantee.
const (
	Proven  = "proven"
	Assumed = "assumed"
)

// Service is one named, loaded service graph in the fleet.
type Service struct {
	Name  string
	Index *graph.Index
}

// End is one service's proven contribution to a chain: the publisher (producer
// side) or consumer-handler (consumer side) function, with the facts the graph
// proves about it. Facts are pre-rendered, decision-relevant lines.
type End struct {
	Service string
	Fns     []string
	Facts   []string
}

// BrokerLink is the assumed link: the declared broker guarantee, printed
// verbatim. Signed is the human warrant — false means the values are declared
// but no one has put their name behind them yet.
type BrokerLink struct {
	Name       string
	Decl       policy.Broker
	Signed     bool
	Undeclared bool     // no printable "bus" guarantee was selected
	Unselected []string // brokers that were declared but not named "bus" (so not printed)
}

// Card is one event's cross-service chain.
type Card struct {
	Event     string
	Producers []End
	Broker    BrokerLink
	Consumers []End
	Open      string // non-empty when the chain has no producer or no consumer in the fleet
}

// Report is every chain card the fleet yields, plus the per-service disclosure
// of dynamically-named bus effects (events no card can name).
type Report struct {
	Cards   []Card
	Dynamic map[string]int // service name -> count of <dynamic> bus effects
}

// Build joins the fleet's publishers to consumers by event name and composes a
// card per event. brokers is the declared bus guarantee(s) (may be nil); the
// single broker block — preferring one named "bus" — is printed as every card's
// assumed link.
func Build(fleet []Service, brokers map[string]policy.Broker) *Report {
	link := selectBroker(brokers)

	type side struct {
		services map[string][]string // service -> fns
	}
	pub := map[string]*side{}
	con := map[string]*side{}
	dynamic := map[string]int{}

	add := func(m map[string]*side, event, service, fn string) {
		s := m[event]
		if s == nil {
			s = &side{services: map[string][]string{}}
			m[event] = s
		}
		if fn != "" {
			s.services[service] = append(s.services[service], fn)
		} else if _, ok := s.services[service]; !ok {
			s.services[service] = nil
		}
	}

	for _, svc := range fleet {
		ix := svc.Index
		effects, dyn := ix.BusEffects()
		dynamic[svc.Name] = dyn
		for _, be := range effects {
			switch be.Op {
			case graph.BusPublish:
				add(pub, be.Event, svc.Name, be.From)
			case graph.BusConsume:
				add(con, be.Event, svc.Name, be.From)
			}
		}
		// A consumer entrypoint names the handler even when the CONSUME edge is
		// dynamic — the topic→handler join flowmap recovered.
		for _, ep := range ix.Entrypoints() {
			if ep.Kind == "consumer" {
				add(con, ep.Name, svc.Name, ep.Fn)
			}
		}
	}

	byName := map[string]*graph.Index{}
	for _, svc := range fleet {
		byName[svc.Name] = svc.Index
	}

	events := map[string]bool{}
	for ev := range pub {
		events[ev] = true
	}
	for ev := range con {
		events[ev] = true
	}

	var cards []Card
	for _, ev := range setutil.SortedKeys(events) {
		card := Card{Event: ev, Broker: link}
		if s := pub[ev]; s != nil {
			for _, name := range setutil.SortedKeys(s.services) {
				card.Producers = append(card.Producers, producerEnd(name, ev, byName[name], s.services[name]))
			}
		}
		if s := con[ev]; s != nil {
			for _, name := range setutil.SortedKeys(s.services) {
				card.Consumers = append(card.Consumers, consumerEnd(name, ev, byName[name], s.services[name]))
			}
		}
		switch {
		case len(card.Producers) == 0:
			card.Open = "no producer in the loaded fleet — chain is open upstream"
		case len(card.Consumers) == 0:
			card.Open = "no consumer in the loaded fleet — chain is open downstream"
		}
		cards = append(cards, card)
	}
	return &Report{Cards: cards, Dynamic: dynamic}
}

// selectBroker picks the one bus guarantee to print: prefer the key "bus", else
// the sole entry; with several and no "bus" it declines rather than guess —
// disclosing the declared-but-unselected names rather than reading as if no
// broker were configured at all.
func selectBroker(brokers map[string]policy.Broker) BrokerLink {
	if len(brokers) == 0 {
		return BrokerLink{Undeclared: true}
	}
	if b, ok := brokers["bus"]; ok {
		return BrokerLink{Name: "bus", Decl: b, Signed: b.Signed()}
	}
	if len(brokers) == 1 {
		for name, b := range brokers {
			return BrokerLink{Name: name, Decl: b, Signed: b.Signed()}
		}
	}
	return BrokerLink{Undeclared: true, Unselected: setutil.SortedKeys(brokers)}
}

// producerEnd gathers the proven producer-side facts for an event: the publish's
// commit ordering (effect_order — certainly vs possibly committed before a
// fault) and any must-precede verdicts on the publishing functions.
func producerEnd(service, event string, ix *graph.Index, fns []string) End {
	e := End{Service: service, Fns: dedupe(fns)}
	pubFns := setutil.StringSet(e.Fns)
	for _, f := range ix.EffectOrder() {
		// The chain card's producer fact is the IN-FRAME publish ordering: at a
		// function that itself publishes this event, is the publish committed
		// before its own fallible work? Transitive carrier facts (a caller far
		// upstream whose carrier call happens to dominate the publish) are a
		// triage concern and would swamp the card — exclude them by requiring the
		// publish to be made by this very function.
		if !namesEvent(f.Effect, graph.BusPublish, event) || !pubFns[f.Fn] {
			continue
		}
		when := "possibly committed"
		if f.Always {
			when = "CERTAINLY committed"
		}
		via := ""
		if f.Via != "" {
			via = " (via " + short(f.Via) + ")"
		}
		e.Facts = append(e.Facts, fmt.Sprintf("%s %s before fallible %s%s",
			short(f.Fn), when, short(f.Callee), via))
	}
	for _, o := range ix.Obligations() {
		if o.Kind == "must-precede" && pubFns[o.Fn] {
			e.Facts = append(e.Facts, fmt.Sprintf("%s: %s at %s", o.Rule, o.Status, short(o.Fn)))
		}
	}
	sort.Strings(e.Facts)
	return e
}

// consumerEnd gathers the proven consumer-side facts for an event: the handler's
// downstream boundary effects (what it commits on receipt) and any obligations
// over the handler and its cone.
func consumerEnd(service, event string, ix *graph.Index, fns []string) End {
	e := End{Service: service, Fns: dedupe(fns)}
	// cone is the handler(s) plus everything reachable from them — seeded with
	// the handlers themselves, so iterating it covers both without re-visiting.
	cone := setutil.StringSet(e.Fns)
	for _, fn := range e.Fns {
		for _, r := range ix.Reachable(fn) {
			cone[r] = true
		}
	}
	seen := map[string]bool{}
	for _, fn := range setutil.SortedKeys(cone) {
		for _, eff := range ix.Effects(fn) {
			// Downstream-committed effects only: skip the inbound consume itself
			// (it is the chain's join, not something the handler commits) and any
			// unresolved frontier edge.
			if eff.IsDynamic() || !eff.IsBoundary() || eff.Boundary == "inbound" {
				continue
			}
			label := strings.TrimPrefix(eff.To, "boundary:")
			if !seen[label] {
				seen[label] = true
				e.Facts = append(e.Facts, "commits "+label)
			}
		}
	}
	for _, o := range ix.Obligations() {
		if cone[o.Fn] {
			e.Facts = append(e.Facts, fmt.Sprintf("%s (%s): %s at %s", o.Rule, o.Kind, o.Status, short(o.Fn)))
		}
	}
	sort.Strings(e.Facts)
	return e
}

// namesEvent reports whether a boundary-effect label names the given bus op and
// event, e.g. ("boundary:bus PUBLISH loan.approved", PUBLISH, "loan.approved").
func namesEvent(label, op, event string) bool {
	return strings.HasSuffix(label, "bus "+op+" "+event)
}

// short renders an FQN compactly for a card line — dropping the module path,
// pointer star, and receiver parens (e.g. "origination.Evaluator.Evaluate").
// The same display convention groundwork's other cards use; the exact FQN still
// lives in the graph.
func short(fqn string) string {
	s := strings.ReplaceAll(strings.TrimPrefix(fqn, "("), "*", "")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return strings.ReplaceAll(s, ")", "")
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return setutil.SortedKeys(setutil.StringSet(in))
}
