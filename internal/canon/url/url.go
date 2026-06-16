// Package url parameterizes request paths into route templates (canon spec
// §3.4). Raw paths carry volatile ids — /score/8412, /loans/3f2a… — that would
// make every run a different golden, so the canonicalizer prefers an explicit
// route attribute when the instrumentation provides one and otherwise falls back
// to segment parameterization: numeric and UUID-shaped path segments become
// {id}, leaving the structural shape /score/{id}.
package url

import (
	"regexp"
	"strings"
)

var (
	numeric = regexp.MustCompile(`^\d+$`)
	uuid    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	longhex = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
)

// Template returns a route template for path. A path is first stripped of any
// scheme/host and query string, then each segment that looks like an identifier
// (all-numeric, a UUID, or a long hex token) is replaced with {id}. Segments
// already in {brace} form are left untouched.
func Template(path string) string {
	path = stripHostAndQuery(path)
	if path == "" {
		return path
	}
	lead := strings.HasPrefix(path, "/")
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, s := range segs {
		if IsID(s) {
			segs[i] = "{id}"
		}
	}
	out := strings.Join(segs, "/")
	if lead {
		out = "/" + out
	}
	return out
}

// stripHostAndQuery reduces a full or relative URL to its path component. Query
// and fragment are cut first, then any scheme://host prefix is stripped
// REPEATEDLY: a malformed path can carry more than one (":///://x"), and a single
// pass would leave a "://" behind — breaking Template's idempotence (the residual
// scheme re-strips on the next pass) and its no-scheme guarantee. Looping to a
// fixed point (FuzzTemplateInvariants) keeps the canonical form stable.
func stripHostAndQuery(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = raw[:i]
	}
	for {
		i := strings.Index(raw, "://")
		if i < 0 {
			break
		}
		rest := raw[i+3:]
		j := strings.IndexByte(rest, '/')
		if j < 0 {
			return "/"
		}
		raw = rest[j:]
	}
	return raw
}

// IsID reports whether a token looks like a volatile identifier: all-numeric, a
// UUID, or a long (16+) hex run. This is the conservative, unambiguous id rule shared
// by route templating and messaging-destination templating, so the two never diverge.
// A token already in {brace} form is left untouched.
func IsID(s string) bool {
	if s == "" || (strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")) {
		return false
	}
	return numeric.MatchString(s) || uuid.MatchString(s) || longhex.MatchString(s)
}
