// Package fitness evaluates a policy against a graph index and returns
// deterministic findings — the architectural invariants that fail closed in CI.
// It is the first verdict-bearing surface: layering, must-not-reach (three-valued
// so an over-approximated "no path" is never disguised as a proof), and the
// per-route I/O budget. Every finding names the exact edge or symbol it fires on.
package fitness

import (
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
)

// PkgOf returns the import path of the package that declares the function named
// by an ssa-style FQN. It handles the two shapes flowmap emits:
//
//	example.com/svc/internal/app.Do                  (free function)
//	(*example.com/svc/internal/app.Service).Do       (method; receiver may be a value)
//
// Type arguments on a generic receiver or function (a "[...]" suffix) are
// stripped before the package is read off. An FQN that does not parse yields "".
func PkgOf(fqn string) string {
	s := fqn
	if strings.HasPrefix(s, "(") {
		end := strings.IndexByte(s, ')')
		if end < 0 {
			return ""
		}
		s = strings.TrimPrefix(s[1:end], "*")
	}
	return pkgFromQualified(stripTypeArgs(s))
}

// pkgFromQualified reads the package path off a qualified name "<pkgpath>.<sym>".
// The symbol separator is the first '.' in the final '/'-segment, because a Go
// package's final path element is an identifier (no dot), while the path prefix
// may contain dots (example.com).
func pkgFromQualified(s string) string {
	prefix, seg := "", s
	if slash := strings.LastIndexByte(s, '/'); slash >= 0 {
		prefix, seg = s[:slash+1], s[slash+1:]
	}
	dot := strings.IndexByte(seg, '.')
	if dot < 0 {
		return s
	}
	return prefix + seg[:dot]
}

// stripTypeArgs removes a "[...]" generic type-argument suffix, which can itself
// contain '/' and '.' and would otherwise confuse package extraction.
func stripTypeArgs(s string) string {
	if i := strings.IndexByte(s, '['); i >= 0 {
		return s[:i]
	}
	return s
}

// ShortName renders an FQN compactly for summaries — it drops the module path
// prefix, the pointer star and the receiver parens, leaving e.g.
// "handler.Server.UpdateUser" or "layeredsvc.run". The exact FQN is still carried
// in a finding's From/To; this is display only.
func ShortName(fqn string) string {
	s := strings.ReplaceAll(strings.TrimPrefix(fqn, "("), "*", "")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return strings.ReplaceAll(s, ")", "")
}

// MatchesAny is the exported form of the rule-pattern matcher, for surfaces
// (the ground card) that must answer "does this rule bind this symbol?" with
// EXACTLY the semantics the checks use — a second matcher is how a card
// promises a guardrail that does not actually bind.
func MatchesAny(s string, patterns []string) bool { return matchAny(s, patterns) }

// matchAny reports whether s equals any pattern or is bound by one as a prefix
// AT AN IDENTIFIER BOUNDARY. A prefix binds only when the byte of s immediately
// after the pattern is a non-identifier byte (e.g. '.', '$', '/', '[', ' '), so a
// pattern can name a function, a receiver type, a whole package, or a boundary
// system and bind all its members — `(*pkg.T)` → `(*pkg.T).M`, `pkg` → `pkg.Fn`,
// `Fn` → its generated closures `Fn$1`, `boundary:bus PUBLISH` → `... orders` —
// without a textual prefix that splits an identifier and binds an UNRELATED symbol.
// The bare-HasPrefix form let `GetUser` bind the distinct route `GetUserAvatar`
// (and a package `app` bind `apps`): the prefix-collision that let init's read-only
// proposal sweep in a sibling route's writes and mis-attribute its effects (R7
// review). An exact match always binds.
func matchAny(s string, patterns []string) bool {
	for _, p := range patterns {
		if s == p {
			return true
		}
		if len(s) > len(p) && strings.HasPrefix(s, p) && !isIdentByte(s[len(p)]) {
			return true
		}
	}
	return false
}

// isIdentByte reports whether b can appear inside a Go identifier, so a prefix
// ending immediately before it would split that identifier rather than bind it at
// a boundary. FQNs and boundary labels here are ASCII.
func isIdentByte(b byte) bool {
	return b == '_' ||
		('a' <= b && b <= 'z') ||
		('A' <= b && b <= 'Z') ||
		('0' <= b && b <= '9')
}

// bindFroms is the ONE entry by which a From-bearing rule kind
// (must_not_reach, must_pass_through, and any future rule) binds its sources:
// it expands the From selectors and, when the whole From binds nothing,
// discloses the inert rule on the result and returns nil. Expansion and
// disclosure are fused deliberately — a check that expands without the
// disclosure is how a renamed package turns a safety rule into a silent
// forever-pass, the inert-guardrail failure mode.
func bindFroms(ix *graph.Index, r *Result, kind, name string, from []string, requireProof bool) []string {
	froms := expandFroms(ix, from)
	if len(froms) == 0 {
		r.add(inertRuleFinding(kind, name, requireProof))
		return nil
	}
	return froms
}

// expandFroms expands a rule's From selectors against the graph: the
// "entrypoint:*" selector matches every graph source, anything else is an FQN
// exact-or-prefix pattern. The union is sorted and de-duplicated. Checks call
// bindFroms, not this, so the inert-rule disclosure cannot be forgotten.
func expandFroms(ix *graph.Index, patterns []string) []string {
	var pats []string
	entry := false
	for _, p := range patterns {
		if p == policy.EntrypointSelector {
			entry = true
		} else {
			pats = append(pats, p)
		}
	}
	set := map[string]bool{}
	if entry {
		for _, s := range ix.Sources() {
			set[s] = true
		}
	}
	for _, fqn := range matchNodes(ix, pats) {
		set[fqn] = true
	}
	return setutil.SortedKeys(set)
}
