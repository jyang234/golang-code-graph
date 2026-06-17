package impeach

import (
	"strings"
	"testing"
)

// FuzzCanonFQNSymmetry is the ⊥-symmetry fuzz the plan requires before L1 may ever
// trust `absent-from-graph` (§7 fail-closed property 1, §10 cross-cutting, §12.5):
// over GENERATED FQNs of each reconcilable class, the ssa node spelling and the
// runtime tag spelling must canonFQN IDENTICALLY — both to the same key, or both
// ⊥. Asymmetry (one side keys, the other ⊥s) is exactly the bug that would mint a
// phantom missing node, so this drives toward proving it cannot happen over the
// realistic domain (clean-identifier package segments; the dotted-final-segment
// gap is the documented L2-only carve-out, so the generator stays inside the
// domain symmetry is claimed for).
func FuzzCanonFQNSymmetry(f *testing.F) {
	f.Add("example.com/a/b", "Bureau", "Score", uint8(0))
	f.Add("svc/internal/origination", "", "NewEvaluator", uint8(3))
	f.Add("p", "T", "M", uint8(1))
	f.Fuzz(func(t *testing.T, pkgSeed, recvSeed, nameSeed string, sel uint8) {
		pkg := cleanPkgPath(pkgSeed)
		recv := cleanIdent(recvSeed)
		name := cleanIdent(nameSeed)
		if pkg == "" || name == "" {
			return // not a well-formed function name; nothing to reconcile
		}

		var ssa, runtime string
		switch sel % 4 {
		case 0: // pointer method
			if recv == "" {
				return
			}
			ssa = "(*" + pkg + "." + recv + ")." + name
			runtime = pkg + ".(*" + recv + ")." + name
		case 1: // value method
			if recv == "" {
				return
			}
			ssa = "(" + pkg + "." + recv + ")." + name
			runtime = pkg + "." + recv + "." + name
		default: // package-level func (spelled identically on both sides)
			ssa = pkg + "." + name
			runtime = ssa
		}

		ks, oks := canonFQN(ssa)
		kr, okr := canonFQN(runtime)
		if oks != okr {
			t.Fatalf("⊥-asymmetry: ssa %q ok=%v vs runtime %q ok=%v", ssa, oks, runtime, okr)
		}
		if oks && ks != kr {
			t.Fatalf("keys disagree: ssa %q -> %+v vs runtime %q -> %+v", ssa, ks, runtime, kr)
		}
	})
}

// FuzzCanonFQNTotal pins totality (§7 fail-closed property 3): canonFQN never
// panics on ARBITRARY input and stays pure, and a parseable input never carries a
// ⊥ reason. This is the robustness half — the symmetry fuzz above generates
// well-formed pairs; this one feeds raw bytes straight in.
func FuzzCanonFQNTotal(f *testing.F) {
	for _, s := range []string{"", "(", ".func1", "a.b.c.d", "(*p.T).M-fm", "x[go.shape.int]"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		k1, ok1 := canonFQN(raw)
		k2, ok2 := canonFQN(raw)
		if k1 != k2 || ok1 != ok2 {
			t.Fatalf("impure on %q", raw)
		}
		if ok1 {
			if fqnBotReason(raw) != "" {
				t.Fatalf("parseable %q carries a ⊥ reason", raw)
			}
			// A parsed key never embeds the structural separators it split on.
			if strings.ContainsAny(k1.Name, ".()*") || strings.ContainsAny(k1.Recv, "()*") {
				t.Fatalf("leaky key on %q: %+v", raw, k1)
			}
		} else if fqnBotReason(raw) == "" {
			t.Fatalf("⊥ %q carries no reason", raw)
		}
	})
}

// cleanIdent maps a fuzz seed to a Go-identifier-ish token (letters only,
// non-empty becomes "" only when the seed has no letters), so generated names
// stay inside the domain canonFQN's split rules are exact for.
func cleanIdent(seed string) string {
	var b strings.Builder
	for _, r := range seed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		}
		if b.Len() >= 8 {
			break
		}
	}
	return b.String()
}

// cleanPkgPath builds a slash-separated package path whose FINAL segment is a
// clean identifier (no dot) — the domain symmetry is claimed for. A FIXED dotted
// domain ("ex.com") is prepended in non-final position so the generator still
// exercises a dotted package path (the robust last-'.' receiver split) WITHOUT
// reopening the dotted-final-segment gap (§7/§12.5, the L2-only carve-out).
func cleanPkgPath(seed string) string {
	var segs []string
	for _, p := range strings.Split(seed, "/") {
		if id := cleanIdent(p); id != "" {
			segs = append(segs, id)
		}
	}
	if len(segs) == 0 {
		return ""
	}
	return "ex.com/" + strings.Join(segs, "/")
}
