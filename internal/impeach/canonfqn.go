package impeach

import "strings"

// canonFQN (plan §7) is the ONE helper that reconciles a function's two spellings
// — the ssa node FQN the graph carries (ssa.Function.RelString: a package func
// "…/origination.NewEvaluator", a pointer method "(*…/client.Bureau).Score", a
// value method "(…/client.Bureau).Score") and the runtime FQN an L1 capture tag
// carries (runtime.FuncForPC.Name: "…/origination.NewEvaluator", the
// re-parenthesized "…/client.(*Bureau).Score" / "…/client.Bureau.Score") — into a
// single FQNKey whose equality means "the SAME function".
//
// It is TOTAL and PURE: every input yields either a key or (⊥, recorded reason),
// with no clock, no map iteration, no side effect, so it is deterministic by
// construction (§7 fail-closed property 3). ⊥ is an HONEST "these spellings have no
// stable correspondence", never a guess: closures ($N vs .funcN), generic
// instantiations (concrete vs go.shape), method-value thunks ($bound / -fm), and
// package init have divergent or synthetic names on the two sides, so a key that
// claimed to reconcile them could mis-anchor (§7's ⊥ policy). The severance map
// (spanmap.go) absorbs every ⊥ as an un-anchored gap.
//
// SOUNDNESS CAVEAT (§7 fail-closed property 1, §12.5): the pkg/symbol split for an
// UNPARENTHESIZED runtime spelling (the value-method vs package-func ambiguity)
// uses the first '.' after the last '/', which is only correct when the package
// path's final segment is a clean identifier. For an exotic dotted-final-segment
// import path (gopkg.in/yaml.v3) the two spellings can split differently —
// ⊥-asymmetry — which is exactly why the sharp `absent-from-graph` signal it feeds
// is trusted only at L2 until the symmetry fuzz proves it over a realistic FQN
// domain. The parenthesized (ssa) and `.(*` (runtime ptr-method) forms split the
// receiver at its LAST '.', which is robust to dotted paths, so methods reconcile
// symmetrically regardless.

// FQNKey identifies a function up to spelling: two spellings of the same function
// canonicalize to the same key, and equality is value equality. Recv is "" for a
// package-level function; Ptr distinguishes a pointer receiver from a value one
// (they are different methods).
type FQNKey struct {
	Pkg  string // defining package import path
	Recv string // receiver type name (package-relative), "" for a package-level func
	Ptr  bool   // pointer receiver
	Name string // function or method identifier
}

// canonFQN parses raw into its FQNKey, or returns ok=false (⊥) when the two
// spellings have no stable correspondence. It is the plan §7 signature: total,
// pure, (_, false) == ⊥.
func canonFQN(raw string) (FQNKey, bool) {
	k, _, ok := parseFQN(raw)
	return k, ok
}

// fqnBotReason returns WHY raw is ⊥ (for the witness disclosure, §7: "each
// recorded with a reason that rides into the witness"), or "" when raw parses. It
// shares parseFQN so the reason can never drift from the decision.
func fqnBotReason(raw string) string {
	_, reason, ok := parseFQN(raw)
	if ok {
		return ""
	}
	return reason
}

// parseFQN is the shared worker behind canonFQN and fqnBotReason: it returns the
// key, the ⊥ reason (empty when ok), and whether the parse succeeded.
func parseFQN(raw string) (FQNKey, string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return FQNKey{}, "empty", false
	}
	// Non-reconcilable synthetic / divergent shapes, refused before any split so a
	// closure or thunk can never be mis-keyed onto a real function (§7 ⊥ policy).
	switch {
	case strings.Contains(raw, "["):
		// A generic instantiation: ssa "Map[int]" vs runtime "Map[go.shape.int]" —
		// concrete vs shape, no correspondence (L2 makes these exact, §7/§12.3).
		return FQNKey{}, "generic instantiation (concrete vs go.shape)", false
	case strings.Contains(raw, "$"):
		// An ssa synthetic: a "$N" closure or a "$bound"/"$thunk" method value.
		return FQNKey{}, "ssa synthetic ($N closure / $bound thunk)", false
	case strings.HasSuffix(raw, "-fm"):
		// A runtime method-value thunk (T.M-fm) — no ssa counterpart node.
		return FQNKey{}, "runtime method value (-fm thunk)", false
	case hasRuntimeClosure(raw):
		// A runtime closure (Parent.funcN) — the lexical-parent correspondence the
		// frontier classifier reconstructs is not a function identity (§7).
		return FQNKey{}, "runtime closure (.funcN)", false
	}

	// Parenthesized ssa receiver: "(<*?recv>).<Name>". The receiver path is split
	// at its LAST '.', so a dotted package path stays whole (robust, see caveat).
	if strings.HasPrefix(raw, "(") {
		idx := strings.Index(raw, ").")
		if idx < 0 {
			return FQNKey{}, "malformed parenthesized receiver", false
		}
		inner := raw[1:idx]
		name := raw[idx+2:]
		if name == "" || strings.ContainsAny(name, ".(") {
			return FQNKey{}, "malformed method name", false
		}
		ptr := strings.HasPrefix(inner, "*")
		inner = strings.TrimPrefix(inner, "*")
		pkg, recv, ok := splitLastDot(inner)
		if !ok || pkg == "" || !validSegment(recv) || !validSegment(name) {
			return FQNKey{}, "malformed parenthesized receiver", false
		}
		if name == "init" {
			return FQNKey{}, "package init", false
		}
		return FQNKey{Pkg: pkg, Recv: recv, Ptr: ptr, Name: name}, "", true
	}

	// Runtime pointer method: "<pkg>.(*<Type>).<Name>". The ".(*" marker gives the
	// package boundary unambiguously, so this is robust to dotted paths.
	if i := strings.Index(raw, ".(*"); i >= 0 {
		pkg := raw[:i]
		rest := raw[i+len(".(*"):]
		j := strings.Index(rest, ").")
		if j < 0 {
			return FQNKey{}, "malformed runtime pointer method", false
		}
		recv := rest[:j]
		name := rest[j+2:]
		if pkg == "" || !validSegment(recv) || !validSegment(name) {
			return FQNKey{}, "malformed runtime pointer method", false
		}
		if name == "init" {
			return FQNKey{}, "package init", false
		}
		return FQNKey{Pkg: pkg, Recv: recv, Ptr: true, Name: name}, "", true
	}

	// Unparenthesized: a package-level func ("<pkg>.<Name>") or a runtime VALUE
	// method ("<pkg>.<Type>.<Name>"). Distinguished by the package boundary (first
	// '.' after the last '/') then the trailing-segment count — the split that is
	// only sound for a clean final path segment (the documented L1 gap).
	pkg, sym, ok := splitPkgSymbol(raw)
	if !ok || pkg == "" {
		return FQNKey{}, "no package separator", false
	}
	parts := strings.Split(sym, ".")
	switch len(parts) {
	case 1:
		if !validSegment(parts[0]) {
			return FQNKey{}, "malformed symbol", false
		}
		if parts[0] == "init" {
			return FQNKey{}, "package init", false
		}
		return FQNKey{Pkg: pkg, Name: parts[0]}, "", true
	case 2:
		if !validSegment(parts[0]) || !validSegment(parts[1]) {
			return FQNKey{}, "malformed symbol", false
		}
		if parts[1] == "init" {
			return FQNKey{}, "package init", false
		}
		return FQNKey{Pkg: pkg, Recv: parts[0], Ptr: false, Name: parts[1]}, "", true
	default:
		return FQNKey{}, "ambiguous symbol (more than two trailing segments)", false
	}
}

// validSegment reports whether s is a plausible package-relative identifier (a
// receiver type or function name): non-empty and free of the structural
// characters the FQN grammar uses as delimiters. It never tries to fully validate
// a Go identifier — it only rejects the leaky shapes (stray parens/stars/dots/
// slashes from malformed input) that would otherwise ride into a key.
func validSegment(s string) bool {
	return s != "" && !strings.ContainsAny(s, "()*./")
}

// hasRuntimeClosure reports whether raw carries a runtime closure suffix segment
// ".funcN" (optionally ".funcN.M") — a '.', "func", one or more digits, then a '.'
// or end of string. The digit requirement keeps a real identifier like
// "funcRegistry" from reading as a closure.
func hasRuntimeClosure(raw string) bool {
	const marker = ".func"
	from := 0
	for {
		i := strings.Index(raw[from:], marker)
		if i < 0 {
			return false
		}
		j := from + i + len(marker)
		digits := 0
		for j < len(raw) && raw[j] >= '0' && raw[j] <= '9' {
			j++
			digits++
		}
		if digits > 0 && (j == len(raw) || raw[j] == '.') {
			return true
		}
		from = from + i + len(marker)
	}
}

// splitLastDot splits s at its LAST '.', returning (before, after). Used for a
// receiver path "<pkg>.<Type>" where Type is the final segment: robust to dots
// inside the package path. ok is false when s has no '.'.
func splitLastDot(s string) (string, string, bool) {
	i := strings.LastIndexByte(s, '.')
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// splitPkgSymbol splits a non-parenthesized FQN into (packagePath, symbol) at the
// first '.' AFTER the last '/'. ok is false when there is no '.' in the final path
// segment. This is the boundary rule that is only exact for a clean final segment
// (the §7/§12.5 caveat); it is deliberately the SAME rule on both spellings so a
// package-level func — spelled identically on both sides — always reconciles.
//
// ONE SOURCE OF TRUTH (CLAUDE.md): this is the same package-boundary predicate as
// fitness.pkgFromQualified (internal/groundwork/fitness/fqn.go) — first '.' after
// the last '/', because a package's final path segment is a dot-free identifier. It
// is not shared as a call because canonFQN needs the SYMBOL remainder too and must
// also parse runtime spellings fitness never sees; instead the parity with
// fitness.PkgOf is pinned by TestCanonFQNPackageParityWithFitness so the two copies
// cannot silently drift.
func splitPkgSymbol(s string) (string, string, bool) {
	slash := strings.LastIndexByte(s, '/')
	dot := strings.IndexByte(s[slash+1:], '.')
	if dot < 0 {
		return "", "", false
	}
	cut := slash + 1 + dot
	return s[:cut], s[cut+1:], true
}
