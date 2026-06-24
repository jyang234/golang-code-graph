package reviewtriage

// This file holds the reviewer-legible --summary render. The other renders
// (RenderMarkdown, RenderMermaid) speak the analyzer's epistemics — counts of
// "newly unverifiable" seams, flat tiers. That is exact, but on a real diff it leads
// with a number that reads as alarm where the change is routine telemetry, and buries
// the one catch a human must act on (an instrumentation wrapper that makes a live DB
// call read as a DROPPED effect). RenderSummary reshapes the SAME computed Report — no
// new analysis, --json unchanged — so it lands for a reviewer who has never touched the
// tool:
//
//   - Classify each newly-blind seam by WHY it is blind, from its Kind and (for an
//     external handoff) its third-party Package: an instrumentation wrapper, a routine
//     telemetry/cache handoff, an unknown external package, a runtime-chosen dispatch,
//     or a fully-unresolved callee.
//   - Promote the decision-relevant classes to plain-language callouts (masking first),
//     and AGGREGATE the routine telemetry/cache handoffs into one skimmable line.
//   - FOLD — never truncate — the long tail (full by-tier list, carried, accounted) into
//     <details>, so nothing is dropped from the comment.
//   - Reframe ⚠️ with a one-line legend: it marks where the tool STOPS seeing; the call is
//     the reviewer's, not a bug the tool found.
//
// Fail-loud is the load-bearing rule (CLAUDE.md tenets 2-3): ONLY a package on the fixed
// telemetry/cache allowlist is aggregated into the routine line. Every other package —
// and every unknown seam kind — is SURFACED as a promoted callout. Hiding is the
// dangerous direction here (a write-heavy handler reachable only through an unrecognized
// package must never vanish into "routine"), so the allowlist is deliberately small and
// the default is to promote.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
	"github.com/jyang234/golang-code-graph/internal/sqlverb"
)

// Seam classes — the WHY-blind taxonomy the summary orders attention by. Every value
// except classTelemetry is PROMOTED (surfaced as a callout); classTelemetry is the only
// class that aggregates into the routine line. An unknown seam kind falls to
// classUnresolved (surfaced), never silently to classTelemetry.
const (
	classMasking    = "masking"    // ExternalBoundaryCall into an instrumentation wrapper (otelsql/…)
	classTelemetry  = "telemetry"  // ExternalBoundaryCall into a known telemetry/cache package (routine)
	classExternal   = "external"   // ExternalBoundaryCall into any other third-party package (surfaced)
	classRuntime    = "runtime"    // dynamic destination/dispatch — op seen, target not (surfaced)
	classUnresolved = "unresolved" // func value / reflection / bypass with no visible callee (surfaced)
)

// instrumentationWrappers are the OpenTelemetry-style wrappers whose presence as a new
// ExternalBoundaryCall seam — paired with a REMOVED effect — reads as instrumentation
// masking: the underlying call is hidden from static analysis, NOT dropped. Each maps to
// the effect DOMAIN token it wraps, so a removed `db …` effect pairs only with an
// otelsql/otelpgx-class wrapper and never a coincidental otelhttp addition (the prototype
// over-paired on a bare "db × otelsql" join; the domain map narrows it). Matched as a
// substring of the seam's Package path. FIXED set: an unrecognized wrapper is NOT treated
// as masking — it falls through to a surfaced external callout (fail-loud).
var instrumentationWrappers = map[string]string{
	"otelsql":   "db",
	"otelpgx":   "db",
	"otelmongo": "db",
	"otelredis": "db",
	"otelhttp":  "http",
	"otelgrpc":  "grpc",
}

// telemetryCachePackages are the routine, low-signal handoff destinations — metrics,
// logging, tracing, and in-process caching/dedup — that an ExternalBoundaryCall into is
// expected plumbing on most diffs. They are matched as a "/"-segment token of the seam's
// Package (so github.com/acme/statsy → "statsy") and aggregated into one "routine — skim"
// line. This is the ONLY class that aggregates, so the set is deliberately conservative:
// when in doubt a package is LEFT OUT and therefore surfaced. State-bearing backends
// (redis, postgres, kafka, s3, …) are intentionally absent — a cache that hits an
// external store is a real effect, not routine.
var telemetryCachePackages = map[string]bool{
	"statsy": true, "statsd": true, "metrics": true, "prometheus": true,
	"obs": true, "observability": true, "telemetry": true,
	"logging": true, "zap": true, "zerolog": true, "tracing": true,
	"singleflight": true, "groupcache": true,
}

// instrWrapperToken returns the instrumentation-wrapper token matched in pkg (and so its
// effect domain via instrumentationWrappers), or "" when pkg is not a known wrapper.
// Deterministic: the wrapper tokens are tested in sorted order so a path that somehow
// matched two always resolves the same one.
func instrWrapperToken(pkg string) string {
	for _, w := range setutil.SortedKeys(instrumentationWrappers) {
		if strings.Contains(pkg, w) {
			return w
		}
	}
	return ""
}

// telemetryToken returns the telemetry/cache token if any "/"-segment of pkg is on the
// allowlist, else "". Keying the routine aggregate on this token (not the full path)
// yields the clean "statsy×38" rollup the FR asks for.
func telemetryToken(pkg string) string {
	for _, seg := range strings.Split(pkg, "/") {
		if telemetryCachePackages[seg] {
			return seg
		}
	}
	return ""
}

// classifySeam answers WHY a blind spot is blind. The default arm is the safety net: an
// unrecognized kind is surfaced as classUnresolved (needs judgment), never folded into
// the routine line.
func classifySeam(b graph.BlindSpot) string {
	switch b.Kind {
	case "ExternalBoundaryCall":
		if instrWrapperToken(b.Package) != "" {
			return classMasking
		}
		if telemetryToken(b.Package) != "" {
			return classTelemetry
		}
		return classExternal
	case "NonConstantBoundaryArg", "DynamicEffect", "HighFanOut", "ConcurrentDispatch":
		return classRuntime
	default:
		// UnresolvedDispatch, UnresolvedCall, reflect, unsafe, cgo, go:linkname,
		// ImpeachmentSeam — and any future/unknown kind: the callee is invisible.
		return classUnresolved
	}
}

// promotionClass is the single bucket a newly-blind function is promoted into, or "" when
// every one of its new seams is routine telemetry/cache (so it folds into the routine line
// rather than a callout). Most-blind wins: a function with any unresolved seam is
// unresolved, else runtime, else external, else (instrumentation-wrapper only) masking. A
// masking-only function is normally REPRESENTED by the report-level masking callout and so
// not rendered as its own group; the caller still surfaces it when no masking callout fired
// (a wrapper with no matching removed effect — fail-loud).
func promotionClass(cf ChangedFn) string {
	var runtime, external, masking bool
	for _, s := range cf.NewSeams {
		switch classifySeam(s) {
		case classUnresolved:
			return classUnresolved
		case classRuntime:
			runtime = true
		case classExternal:
			external = true
		case classMasking:
			masking = true
		}
	}
	switch {
	case runtime:
		return classRuntime
	case external:
		return classExternal
	case masking:
		return classMasking
	default:
		return "" // all telemetry ⇒ routine
	}
}

// maskingCallout is one "an effect reads as removed, but a new instrumentation wrapper is
// hiding it, not dropping it" finding: the removed effects in one domain and the wrapper
// package(s) implicated. Advisory and heuristic by construction (see the legend) — it
// never claims a proof.
type maskingCallout struct {
	Domain   string   // effect domain token ("db", "http", …)
	Effects  []string // the removed effects in that domain
	Wrappers []string // the instrumentation-wrapper tokens newly present
}

// detectMasking joins the verified EffectsRemoved against the new instrumentation-wrapper
// seams: when an effect in domain D disappeared AND a wrapper that wraps D newly appears,
// the effect almost certainly moved BEHIND the wrapper rather than out of the code. This
// is the highest-value catch and the one a reviewer cannot see without hand-joining the
// blind-spot list against the removed-effect list. Deterministic: domains, effects, and
// wrappers are all sorted; nothing reads map iteration order.
func detectMasking(r Report) []maskingCallout {
	if len(r.EffectsRemoved) == 0 {
		return nil
	}
	// Wrapper tokens present anywhere in the new/carried blind seams, grouped by domain.
	domainWrappers := map[string]map[string]bool{}
	for _, cf := range append(append([]ChangedFn(nil), r.NewBlind...), r.Carried...) {
		for _, s := range append(append([]graph.BlindSpot(nil), cf.NewSeams...), cf.CarriedSeams...) {
			if s.Kind != "ExternalBoundaryCall" {
				continue
			}
			if w := instrWrapperToken(s.Package); w != "" {
				d := instrumentationWrappers[w]
				if domainWrappers[d] == nil {
					domainWrappers[d] = map[string]bool{}
				}
				domainWrappers[d][w] = true
			}
		}
	}
	if len(domainWrappers) == 0 {
		return nil
	}
	// Removed effects bucketed by leading domain token ("db postgres" ⇒ "db").
	domainEffects := map[string][]string{}
	for _, e := range r.EffectsRemoved {
		domainEffects[effectDomain(e)] = append(domainEffects[effectDomain(e)], e)
	}
	var out []maskingCallout
	for _, d := range setutil.SortedKeys(domainWrappers) {
		effs := domainEffects[d]
		if len(effs) == 0 {
			continue // a wrapper with no matching removed effect ⇒ no masking claim
		}
		sort.Strings(effs)
		out = append(out, maskingCallout{Domain: d, Effects: effs, Wrappers: setutil.SortedKeys(domainWrappers[d])})
	}
	return out
}

// effectDomain is the leading token of a boundary-effect label ("db SELECT users" ⇒ "db",
// "bus PUBLISH e" ⇒ "bus"), the join key against an instrumentation wrapper's domain.
func effectDomain(effect string) string {
	if i := strings.IndexByte(effect, ' '); i >= 0 {
		return effect[:i]
	}
	return effect
}

// RenderSummary is the reviewer-legible MR-comment digest (GitHub-flavored Markdown). It
// leads with a plain-language framing line and the few spots that need a human judgment
// (masking first), aggregates routine telemetry/cache handoffs into one line, and folds
// everything else into <details> so nothing is truncated. It speaks to a reviewer, not to
// the analyzer; the by-tier / carried / accounted detail and the verified-delta orientation
// are all preserved, just demoted out of the lead. FQNs, effect labels, and seam kinds are
// backtick-wrapped so a <dynamic> label renders literally rather than as stray HTML.
func (r Report) RenderSummary(o Options) string {
	var b strings.Builder
	n, c, a := len(r.NewBlind), len(r.Carried), len(r.Accounted)
	total := n + c + a
	b.WriteString("### 🔍 Review triage — where to spend your attention\n\n")
	if total == 0 {
		b.WriteString("No structural change — the diff did not move the call graph. That is not \"safe\"; verify behavior the usual way.\n")
		return b.String()
	}

	masking := detectMasking(r)
	groups := groupPromoted(r.NewBlind)
	routine := routineHandoffs(r.NewBlind)

	// Render order, most-blind first. The instrumentation-masking group is folded in only
	// when NO report-level masking callout fired — otherwise those functions are already
	// represented by it; when a wrapper is present but no removed effect matched, the group
	// surfaces (fail-loud).
	order := []string{classRuntime, classUnresolved, classExternal}
	if len(masking) == 0 {
		order = append(order, classMasking)
	}
	rendered := 0
	for _, cls := range order {
		if len(groups[cls]) > 0 {
			rendered++
		}
	}
	judgment := len(masking) + rendered

	// Framing line: what the diff is, then how many spots need a judgment call.
	fmt.Fprintf(&b, "**%d function(s) changed.** ", total)
	if routine.total > 0 {
		b.WriteString("Much of this diff is telemetry/cache handoffs the analyzer can't see into (expected). ")
	}
	if judgment == 0 {
		b.WriteString("Nothing in it needs a judgment call from the tool's view — but \"accounted\" is structural completeness, never approval; verify the resolved effects below are the ones you intend.\n")
	} else {
		fmt.Fprintf(&b, "Underneath that, **%d spot(s) need judgment:**\n", judgment)
	}

	// Promoted callouts as numbered blockquotes — masking first (the highest-value catch),
	// then the rest most-blind first. Each is a plain-language sentence plus a "Check:".
	num := 0
	for _, m := range masking {
		num++
		writeMaskingCallout(&b, num, m)
	}
	lim := 0
	if !o.Full {
		lim = o.budget()
	}
	for _, cls := range order {
		if g := groups[cls]; len(g) > 0 {
			num++
			writeGroupCallout(&b, num, cls, g, lim)
		}
	}

	// Routine — one skimmable line of package counts, never per-seam. Sorted by count
	// (desc) then token so it is deterministic.
	if routine.total > 0 {
		fmt.Fprintf(&b, "\n**Routine — skim** (%d telemetry/cache handoff(s)): %s\n", routine.total, routine.render(lim))
	}

	// Verified orientation — what the MR does that the tool CAN vouch for. Kept (it is the
	// floor the ⚠️ items sit above), just below the lead. Reuses the sound effect/entrypoint
	// delta and the per-route write movement.
	writeVerifiedDelta(&b, r, lim)
	writeRouteMovement(&b, r, o, lim)

	// Folded detail — nothing truncated. The full by-tier blind list, carried, and accounted
	// all live here; GitHub renders <details> collapsed.
	writeEffectSurface(&b, r)
	writeBlindByTier(&b, r.NewBlind)
	writeCarriedDetails(&b, r, o)
	writeAccountedDetails(&b, r, o)

	b.WriteString("\n— ⚠️ marks where the tool STOPS seeing: the call there is yours to make, not a bug it found. \"Accounted\" means the tool can show the complete structure, not that it is correct — you still verify. Masking is a heuristic (removed effect × instrumentation wrapper), so confirm rather than assume. `groundwork review-triage --full` for per-function evidence.\n")
	return b.String()
}

// writeMaskingCallout renders the "an effect reads as removed — it likely isn't" finding.
func writeMaskingCallout(b *strings.Builder, num int, m maskingCallout) {
	fmt.Fprintf(b, "\n> ⚠️ %d · A `%s` effect now reads as **removed** — it likely isn't.\n", num, m.Domain)
	fmt.Fprintf(b, "> %s disappears because a new instrumentation wrapper (%s) hides the call from static analysis, not a dropped dependency.\n",
		backtickList(m.Effects, 0), backtickList(m.Wrappers, 0))
	fmt.Fprintf(b, "> Check: the `%s` call still happens the way it did on the base.\n", m.Domain)
}

// writeGroupCallout renders one promoted seam-class group: a plain-language reason plus the
// functions in it (capped + folded, never truncated — the full list is in the by-tier
// <details>).
func writeGroupCallout(b *strings.Builder, num int, cls string, fns []ChangedFn, lim int) {
	names := make([]string, len(fns))
	for i, cf := range fns {
		names[i] = fitness.ShortName(cf.FQN)
	}
	var reason, check string
	switch cls {
	case classRuntime:
		reason = fmt.Sprintf("%d path(s) choose their target at runtime — the tool sees the operation but not the destination.", len(fns))
		check = "each goes where you intend"
	case classUnresolved:
		reason = fmt.Sprintf("%d call(s) the tool can't resolve at all — a func value or reflection with no visible callee; what runs there is invisible.", len(fns))
		check = "what actually runs at each"
	case classMasking:
		reason = fmt.Sprintf("%d call(s) route through an instrumentation wrapper the tool can't see inside (no dropped effect matched it, so it is surfaced rather than assumed routine).", len(fns))
		check = "the wrapped call still performs the effect you intend"
	default: // classExternal
		reason = fmt.Sprintf("%d call(s) hand off to a third-party package the tool can't see inside (not a known telemetry/cache lib).", len(fns))
		check = "the effect each performs is the one you intend"
	}
	fmt.Fprintf(b, "\n> ⚠️ %d · %s\n", num, reason)
	fmt.Fprintf(b, "> Check: %s — %s.\n", check, backtickList(names, lim))
}

// groupPromoted buckets the newly-blind functions by their promotion class (telemetry-only
// functions fold out into the routine line, not a callout). Each bucket keeps the input's
// consequence order (NewBlind is already consequence-sorted).
func groupPromoted(newBlind []ChangedFn) map[string][]ChangedFn {
	groups := map[string][]ChangedFn{}
	for _, cf := range newBlind {
		if cls := promotionClass(cf); cls != "" {
			groups[cls] = append(groups[cls], cf)
		}
	}
	return groups
}

// routineAgg is the routine telemetry/cache handoff rollup: per-token seam counts and the
// grand total.
type routineAgg struct {
	byToken map[string]int
	total   int
}

// routineHandoffs counts the telemetry/cache-classified seams across the newly-blind
// functions, keyed by their matched allowlist token. Seam-level (a function promoted for an
// unrelated seam still contributes its telemetry seams here), matching the FR's "112 of 131
// new seams are routine".
func routineHandoffs(newBlind []ChangedFn) routineAgg {
	agg := routineAgg{byToken: map[string]int{}}
	for _, cf := range newBlind {
		for _, s := range cf.NewSeams {
			if classifySeam(s) == classTelemetry {
				agg.byToken[telemetryToken(s.Package)]++
				agg.total++
			}
		}
	}
	return agg
}

// render formats the routine aggregate as "`statsy`×38 · `obs`×34 …", ordered by count
// (desc) then token (asc) for determinism, capped with a disclosed "…+N more package(s)".
func (a routineAgg) render(lim int) string {
	tokens := setutil.SortedKeys(a.byToken)
	sort.SliceStable(tokens, func(i, j int) bool {
		if ci, cj := a.byToken[tokens[i]], a.byToken[tokens[j]]; ci != cj {
			return ci > cj
		}
		return tokens[i] < tokens[j]
	})
	shown, overflow := tokens, 0
	if lim > 0 && len(tokens) > lim {
		shown, overflow = tokens[:lim], len(tokens)-lim
	}
	parts := make([]string, len(shown))
	for i, t := range shown {
		parts[i] = fmt.Sprintf("`%s`×%d", t, a.byToken[t])
	}
	out := strings.Join(parts, " · ")
	if overflow > 0 {
		out += fmt.Sprintf(" · …+%d more package(s)", overflow)
	}
	return out
}

// writeVerifiedDelta renders the sound "what this MR does" orientation: the boundary-effect
// and entrypoint delta over statically-resolved edges. A FLOOR — the ⚠️ items above are
// where it is incomplete.
func writeVerifiedDelta(b *strings.Builder, r Report, lim int) {
	if len(r.EntrypointsAdded)+len(r.EntrypointsRemoved)+len(r.EffectsAdded)+len(r.EffectsRemoved) == 0 {
		return
	}
	b.WriteString("\n**What this MR does (verified):**\n")
	if len(r.EntrypointsAdded) > 0 {
		fmt.Fprintf(b, "- exposes %d new entrypoint(s): %s\n", len(r.EntrypointsAdded), backtickList(r.EntrypointsAdded, lim))
	}
	if len(r.EntrypointsRemoved) > 0 {
		fmt.Fprintf(b, "- removes %d entrypoint(s): %s\n", len(r.EntrypointsRemoved), backtickList(r.EntrypointsRemoved, lim))
	}
	if len(r.EffectsAdded) > 0 {
		fmt.Fprintf(b, "- adds %d external effect(s): %s\n", len(r.EffectsAdded), backtickList(r.EffectsAdded, lim))
	}
	if len(r.EffectsRemoved) > 0 {
		fmt.Fprintf(b, "- removes %d external effect(s): %s\n", len(r.EffectsRemoved), backtickList(r.EffectsRemoved, lim))
	}
	b.WriteString("_(verified over statically-resolved edges; the ⚠️ items above are where this is incomplete)_\n")
}

// writeRouteMovement renders the per-route write movement (only when a policy was supplied),
// folding the overflow rather than dropping it.
func writeRouteMovement(b *strings.Builder, r Report, o Options, lim int) {
	if len(r.RouteIO) == 0 {
		return
	}
	b.WriteString("\n**Per-route write movement (verified):**\n")
	shown, overflow := r.RouteIO, 0
	if !o.Full && len(r.RouteIO) > o.budget() {
		shown, overflow = r.RouteIO[:o.budget()], len(r.RouteIO)-o.budget()
	}
	for _, rm := range shown {
		var moves []string
		if len(rm.Added) > 0 {
			moves = append(moves, "now writes "+backtickList(rm.Added, lim))
		}
		if len(rm.Removed) > 0 {
			moves = append(moves, "no longer writes "+backtickList(rm.Removed, lim))
		}
		tag := ""
		if rm.Blind {
			tag = " _(frontier blind — may be the model shifting, not the code)_"
		}
		fmt.Fprintf(b, "- `%s` %s%s\n", rm.Route, strings.Join(moves, "; "), tag)
	}
	if overflow > 0 {
		fmt.Fprintf(b, "- …and %d more route(s)\n", overflow)
	}
}

// writeEffectSurface folds the union boundary-effect surface of the changed functions into
// a <details>, split into writes / reads / bus / other so a reviewer sees the state the diff
// can reach. Disclosure-only and best-effort: it pairs with the per-function why-blind tags
// (a blind handler's writes may be hidden), so it is a floor, not a complete inventory.
func writeEffectSurface(b *strings.Builder, r Report) {
	writes, reads, bus, other := classifyEffects(r)
	if len(writes)+len(reads)+len(bus)+len(other) == 0 {
		return
	}
	fmt.Fprintf(b, "\n<details><summary>📊 Effect surface — %d write(s) · %d read(s) · %d bus · %d other</summary>\n\n",
		len(writes), len(reads), len(bus), len(other))
	writeEffectGroup(b, "writes", writes)
	writeEffectGroup(b, "reads", reads)
	writeEffectGroup(b, "bus", bus)
	writeEffectGroup(b, "other", other)
	b.WriteString("\n_A floor — an effect behind a ⚠️ blind spot may not appear here._\n</details>\n")
}

func writeEffectGroup(b *strings.Builder, label string, effs []string) {
	if len(effs) == 0 {
		return
	}
	fmt.Fprintf(b, "- **%s** (%d): %s\n", label, len(effs), backtickList(effs, 0))
}

// classifyEffects gathers the deduped, sorted boundary effects reachable from every changed
// function and bins them: a mutating SQL verb is a write, a SELECT a read, a bus op its own
// bin, everything else "other" (surfaced, never silently a read — fail-loud).
func classifyEffects(r Report) (writes, reads, bus, other []string) {
	seen := map[string]bool{}
	all := append(append(append([]ChangedFn(nil), r.NewBlind...), r.Carried...), r.Accounted...)
	for _, cf := range all {
		for _, e := range cf.Effects {
			if seen[e] {
				continue
			}
			seen[e] = true
			f := strings.Fields(e)
			switch {
			case len(f) >= 2 && f[0] == "db" && sqlverb.Mutating(f[1]):
				writes = append(writes, e)
			case len(f) >= 2 && f[0] == "db" && f[1] == "SELECT":
				reads = append(reads, e)
			case len(f) >= 1 && f[0] == "bus":
				bus = append(bus, e)
			default:
				other = append(other, e)
			}
		}
	}
	sort.Strings(writes)
	sort.Strings(reads)
	sort.Strings(bus)
	sort.Strings(other)
	return writes, reads, bus, other
}

// writeBlindByTier folds the COMPLETE newly-blind list (every function, including the
// telemetry-only ones aggregated above) into a <details>, so the routine aggregate and the
// callout caps never drop a name from the record.
func writeBlindByTier(b *strings.Builder, newBlind []ChangedFn) {
	if len(newBlind) == 0 {
		return
	}
	fmt.Fprintf(b, "\n<details><summary>🔬 All %d newly-blind function(s), by consequence</summary>\n\n", len(newBlind))
	for _, cf := range newBlind {
		fmt.Fprintf(b, "- %s\n", summaryLine(cf, distinctKinds(cf.NewSeams)))
	}
	b.WriteString("\n</details>\n")
}

// writeCarriedDetails folds the carried-blind zone (pre-existing on the path, not this
// diff's fault) into a <details>, capped with a disclosed overflow.
func writeCarriedDetails(b *strings.Builder, r Report, o Options) {
	c := len(r.Carried)
	if c == 0 {
		return
	}
	fmt.Fprintf(b, "\n<details><summary>🟡 Carried blindness — %d (pre-existing on the path, not introduced here)</summary>\n\n", c)
	shown, overflow := r.Carried, 0
	if !o.Full && c > o.budget() {
		shown, overflow = r.Carried[:o.budget()], c-o.budget()
	}
	for _, cf := range shown {
		fmt.Fprintf(b, "- %s\n", summaryLine(cf, distinctKinds(cf.CarriedSeams)))
	}
	if overflow > 0 {
		fmt.Fprintf(b, "- …and %d more\n", overflow)
	}
	b.WriteString("\n</details>\n")
}

// writeAccountedDetails folds the fully-accounted zone into a <details>, rolling up by
// package over budget (the same collapse rule the other renders use). "Accounted" is
// structural completeness, never approval.
func writeAccountedDetails(b *strings.Builder, r Report, o Options) {
	a := len(r.Accounted)
	if a == 0 {
		return
	}
	fmt.Fprintf(b, "\n<details><summary>✅ Fully accounted — %d (complete evidence; structural completeness, not approval)</summary>\n\n", a)
	if o.collapseAccounted(a) {
		for _, rl := range rollupAccounted(r.Accounted) {
			fmt.Fprintf(b, "- `%s` — %d change(s)%s\n", shortPkg(rl.Pkg), rl.Count, effSuffix(rl.Effects))
		}
	} else {
		for _, cf := range r.Accounted {
			fmt.Fprintf(b, "- %s\n", summaryLine(cf, nil))
		}
	}
	b.WriteString("\n</details>\n")
}
