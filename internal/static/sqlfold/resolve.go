package sqlfold

import (
	"go/token"
	"go/types"
	"sort"
	"sync"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/jyang234/golang-code-graph/internal/canon/sql"
	"github.com/jyang234/golang-code-graph/internal/static/features"
)

// maxTableSet bounds a resolved table set: a "finite constant set" that grows past
// this is treated as unresolvable. It keeps the enumeration bounded and stops a
// pathological union from naming dozens of targets — at that width the name is
// noise, so the write target stays honestly dynamic.
const maxTableSet = 8

// resolveTable (phase 2) tries to name a write's dynamic table. The table is the
// first hole in the statement skeleton; resolveTable resolves that hole's value to
// a finite, provably-complete set of compile-time constants, then verifies — per
// candidate — that substituting the constant into the constant prefix lands it in
// the table slot (same verb, a non-empty table). It returns the sorted table set,
// or ok=false to leave the target dynamic.
//
// This only ever runs in the write branch, so it cannot change a verdict: the verb
// already settled read-vs-write. A resolution that is wrong or incomplete can only
// misname/over-list a write target (a precision/over-fire effect on a diff), never
// hide a write — the safe direction.
func resolveTable(frags []frag, prefix, op string) ([]string, bool) {
	var hole ssa.Value
	for _, f := range frags {
		if f.kind == fHole {
			hole = f.val
			break
		}
	}
	if hole == nil {
		return nil, false
	}
	consts, ok := resolveConstSet(hole, map[ssa.Value]bool{})
	if !ok {
		return nil, false
	}
	set := map[string]bool{}
	for _, c := range consts {
		n := sql.Normalize(prefix + c)
		// The substituted constant must land as the table of the SAME statement: a
		// matching verb and a non-empty table. If it does not, the hole was not the
		// table slot, so we cannot soundly name it.
		if n.Operation != op || n.Table == "" {
			return nil, false
		}
		set[n.Table] = true
	}
	if len(set) == 0 {
		return nil, false
	}
	return sortedKeys(set), true
}

// resolveConstSet resolves v to the complete, finite set of compile-time string
// constants it can hold, or ok=false when it cannot prove completeness. It handles
// a constant (singleton), a Phi (the union of its operands), and a struct-field
// load (a whole-program field analysis); anything else abstains.
func resolveConstSet(v ssa.Value, seen map[ssa.Value]bool) ([]string, bool) {
	if v == nil || seen[v] {
		return nil, false
	}
	seen[v] = true
	if s, ok := features.ConstString(v); ok {
		return []string{s}, true
	}
	switch x := v.(type) {
	case *ssa.Phi:
		set := map[string]bool{}
		for _, e := range x.Edges {
			es, ok := resolveConstSet(e, seen)
			if !ok {
				return nil, false
			}
			for _, s := range es {
				set[s] = true
			}
		}
		if len(set) == 0 || len(set) > maxTableSet {
			return nil, false
		}
		return sortedKeys(set), true
	case *ssa.UnOp:
		if x.Op == token.MUL {
			if _, ok := x.X.(*ssa.FieldAddr); ok {
				return resolveField(x)
			}
		}
	}
	return nil, false
}

// resolveField resolves a struct-field load to the complete set of constants the
// field can hold across the whole program, or ok=false. Completeness is the whole
// game: a missed write makes the name a lie, so the analysis ABSTAINS unless every
// possible value is provably one of a finite set of constants. The gates:
//
//	(a) every FieldAddr of this field is used only as the address of a CONSTANT
//	    store or as a load — a non-constant store, or the address escaping (passed
//	    or stored), means a value we cannot see, so abstain;
//	(b) the struct is never overwritten whole (a `*p = T{…}` could set the field
//	    out of view) — abstain if any whole-struct store of this type exists;
//	(c) every allocation of the struct sets the field to a constant BEFORE the
//	    object is used (init-before-escape, see fieldSetBeforeEscape) — otherwise
//	    the zero value "" is reachable, which we cannot name.
func resolveField(load *ssa.UnOp) ([]string, bool) {
	fa, ok := load.X.(*ssa.FieldAddr)
	if !ok {
		return nil, false
	}
	target := structNamed(fa.X.Type())
	if target == nil {
		return nil, false
	}
	idx := fa.Field
	prog := load.Parent().Prog

	// Memoize per (program, struct, field): computeFieldSet is a pure whole-program
	// scan, so caching it is deterministic and collapses the O(sites × program) cost
	// to one scan per DISTINCT field — many DB sites parameterized by the same field
	// (the common shape) then share one result. Keyed on the program so it cannot
	// collide across builds; a process builds few programs.
	key := fieldKey{prog: prog, named: target, idx: idx}
	if v, ok := fieldSetCache.Load(key); ok {
		r := v.(fieldResult)
		return r.set, r.ok
	}
	out, found := computeFieldSet(prog, target, idx)
	fieldSetCache.Store(key, fieldResult{set: out, ok: found})
	return out, found
}

// fieldSetCache memoizes computeFieldSet. The value is a pure function of the key,
// so the cache never changes a result — it only avoids re-scanning. A sync.Map
// keeps it safe if two builds run concurrently (e.g. parallel tests).
var fieldSetCache sync.Map // fieldKey -> fieldResult

type fieldKey struct {
	prog  *ssa.Program
	named *types.Named
	idx   int
}

type fieldResult struct {
	set []string
	ok  bool
}

// computeFieldSet is the whole-program scan behind resolveField (gates a/b/c); see
// resolveField's doc for the completeness argument. Factored out so the result can
// be memoized.
func computeFieldSet(prog *ssa.Program, target *types.Named, idx int) ([]string, bool) {
	set := map[string]bool{}
	var allocs []*ssa.Alloc
	for fn := range ssautil.AllFunctions(prog) {
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				switch x := instr.(type) {
				case *ssa.Alloc:
					if n := structNamed(x.Type()); n != nil && types.Identical(n, target) {
						allocs = append(allocs, x)
					}
				case *ssa.FieldAddr:
					if x.Field != idx {
						continue
					}
					if n := structNamed(x.X.Type()); n == nil || !types.Identical(n, target) {
						continue
					}
					if !scanFieldUses(x, set) { // gate (a)
						return nil, false
					}
				case *ssa.Store:
					// gate (b): a whole-struct overwrite (`*p = T{…}`) could set the field
					// out of view. Match the struct VALUE type exactly — NOT via a
					// pointer-stripping check: storing a *T (capturing a pointer in a
					// closure cell, appending to a []*T) only moves the pointer and is
					// harmless; only storing a T value can rewrite the field unseen.
					if types.Identical(x.Val.Type(), types.Type(target)) {
						return nil, false
					}
				}
			}
		}
	}
	// gate (c): no allocation may leave the field at its zero value when used.
	for _, a := range allocs {
		if !fieldSetBeforeEscape(a, idx) {
			return nil, false
		}
	}
	if len(set) == 0 || len(set) > maxTableSet {
		return nil, false
	}
	return sortedKeys(set), true
}

// scanFieldUses checks gate (a) for one FieldAddr and records the constants stored
// through it. Every referrer must be a constant store to this address or a load;
// anything else (a non-constant store, or the address used as a value) means an
// unseen or non-constant write, so it returns false.
func scanFieldUses(fa *ssa.FieldAddr, set map[string]bool) bool {
	if fa.Referrers() == nil {
		return true
	}
	for _, ref := range *fa.Referrers() {
		switch r := ref.(type) {
		case *ssa.Store:
			if r.Addr != ssa.Value(fa) {
				return false // the field's address is stored elsewhere — it escaped
			}
			s, ok := features.ConstString(r.Val)
			if !ok {
				return false // a non-constant write — the set is not all-constant
			}
			set[s] = true
		case *ssa.UnOp:
			if r.Op != token.MUL {
				return false // a non-load operation on the address
			}
		default:
			return false // the address flows somewhere we do not model — abstain
		}
	}
	return true
}

// fieldSetBeforeEscape is gate (c): the alloc's field idx is set to a constant
// before the object is used for anything other than computing a field address.
// Concretely, a constant store to that field must dominate every non-FieldAddr
// referrer of the alloc (the points where the object is returned, passed, stored,
// or loaded). The constructor idiom — allocate, set fields, return — satisfies it;
// a lazy or conditional initialization does not, so the zero value cannot leak.
func fieldSetBeforeEscape(a *ssa.Alloc, idx int) bool {
	if a.Referrers() == nil {
		return false
	}
	var stores []*ssa.Store
	for _, ref := range *a.Referrers() {
		fa, ok := ref.(*ssa.FieldAddr)
		if !ok || fa.Field != idx || fa.Referrers() == nil {
			continue
		}
		for _, r := range *fa.Referrers() {
			st, ok := r.(*ssa.Store)
			if !ok || st.Addr != ssa.Value(fa) {
				continue
			}
			if _, isConst := features.ConstString(st.Val); isConst {
				stores = append(stores, st)
			}
		}
	}
	if len(stores) == 0 {
		return false // the field is never const-set on this alloc → zero value reachable
	}
	for _, ref := range *a.Referrers() {
		if _, ok := ref.(*ssa.FieldAddr); ok {
			continue // address computations are not uses of the object's value
		}
		if !dominatedByAny(ref, stores) {
			return false
		}
	}
	return true
}

// dominatedByAny reports whether some store dominates the use instruction.
func dominatedByAny(use ssa.Instruction, stores []*ssa.Store) bool {
	for _, st := range stores {
		if instrDominates(st, use) {
			return true
		}
	}
	return false
}

// instrDominates reports whether instruction a dominates instruction b: a's block
// dominates b's, and within one block a precedes b.
func instrDominates(a, b ssa.Instruction) bool {
	ab, bb := a.Block(), b.Block()
	if ab == nil || bb == nil {
		return false
	}
	if ab != bb {
		return ab.Dominates(bb)
	}
	for _, in := range ab.Instrs {
		if in == a {
			return true
		}
		if in == b {
			return false
		}
	}
	return false
}

// structNamed returns the named struct type behind t (stripping one pointer), or
// nil if t is not a (pointer to a) named struct.
func structNamed(t types.Type) *types.Named {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	if !ok {
		return nil
	}
	if _, ok := n.Underlying().(*types.Struct); !ok {
		return nil
	}
	return n
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
