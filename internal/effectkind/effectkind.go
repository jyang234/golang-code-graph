// Package effectkind is the single source of truth for the "method-named outbound"
// boundary-kind tokens (the sqlverb precedent, for boundary kinds instead of SQL
// verbs). The static labeler/features NAME such an effect (object storage, cache,
// non-HTTP RPC) and the groundwork write budget CLASSIFIES it, but the two are
// decoupled — they communicate only through the emitted graph label — so the kind
// set lives here, imported by both, instead of being copied into each and left to
// drift (CLAUDE.md: one source of truth). A kind added here reaches every consumer
// at once.
//
// A "method-named outbound" kind has no readable peer/op/target triple: its label
// is "<kind> <Method>" (exactly two fields) and its write-ness is not inferred from
// the method name (a heuristic that could be silently wrong) but disclosed as
// budget-unenforceable.
package effectkind

const (
	// Blob is object storage (an S3/GCS SDK or wrapper).
	Blob = "blob"
	// Cache is a cache client.
	Cache = "cache"
	// RPC is a non-HTTP RPC peer.
	RPC = "rpc"
)

// methodNamed is the canonical set, kept SORTED so any list derived from it is
// deterministic. It is the one backing list both IsMethodNamed and MethodNamedKinds
// read, so a kind added here reaches every consumer at once.
var methodNamed = []string{Blob, Cache, RPC}

// IsMethodNamed reports whether a boundary-kind token is a method-named outbound
// kind. Callers pass the kind token — the first field of a "boundary:<kind> …" label
// (groundwork) or the token the static helper returns (features).
func IsMethodNamed(kind string) bool {
	for _, k := range methodNamed {
		if k == kind {
			return true
		}
	}
	return false
}

// MethodNamedKinds returns the canonical set, sorted, as a fresh copy.
func MethodNamedKinds() []string {
	return append([]string(nil), methodNamed...)
}
