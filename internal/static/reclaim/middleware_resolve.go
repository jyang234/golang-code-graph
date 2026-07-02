package reclaim

import (
	"go/token"
	"go/types"
	"sort"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// fieldSet is the memoised result of resolving a slice field's complete element set:
// funcs is the union of middleware functions stored into the field anywhere in the
// program, ok is false when any write (or any escape of the field's address) could not be
// proven — in which case the seam stays blind.
type fieldSet struct {
	funcs []*ssa.Function
	ok    bool
}

// resolveSet resolves the middleware slice to its COMPLETE element set, or abstains. A
// field-backed slice (`*siw.HandlerMiddlewares`, the oapi-codegen / strict-server shape) is
// resolved program-wide over every store to that field; a slice built locally in the route
// method (a hand-written `[]MiddlewareFunc{...}`) is traced directly. ok=false means the set
// is not provable (a dynamic source) and the seam must stay UnresolvedCall.
func (r *mwReclaimer) resolveSet(slice ssa.Value) ([]*ssa.Function, bool) {
	if fv, ok := sliceFieldVar(slice); ok {
		fs := r.resolveField(fv)
		return fs.funcs, fs.ok
	}
	// Local construction (not field-backed): trace the slice value directly. nil fieldVar
	// means "no same-field base to fold away" — a same-field append base cannot occur here.
	return r.sliceElems(slice, nil)
}

// sliceFieldVar reports whether slice is loaded from a struct field
// (`*ssa.UnOp(MUL)` of `*ssa.FieldAddr`, optionally behind re-slices), returning the
// field's *types.Var so the program-wide store walk can key on field identity.
// sortFuncsByFQNThenPos orders a recovered function set on intrinsic, run-independent
// keys so the middleware-edge set is byte-identical across runs (the prime directive).
// It breaks FQN ties on Pos() because generic instantiations can share a RelString, so
// the name alone is not a total order — the same tie-break obligations/summaries.go
// NewSummaries uses. The ONE home for this predicate so the field-store and field-load
// resolution sites cannot drift (CLAUDE.md one source of truth).
func sortFuncsByFQNThenPos(fns []*ssa.Function) {
	sort.Slice(fns, func(i, j int) bool {
		if a, b := fns[i].RelString(nil), fns[j].RelString(nil); a != b {
			return a < b
		}
		return fns[i].Pos() < fns[j].Pos()
	})
}

func sliceFieldVar(slice ssa.Value) (*types.Var, bool) {
	v := slice
	for {
		if s, ok := v.(*ssa.Slice); ok {
			v = s.X
			continue
		}
		break
	}
	load, ok := v.(*ssa.UnOp)
	if !ok || load.Op != token.MUL {
		return nil, false
	}
	fa, ok := load.X.(*ssa.FieldAddr)
	if !ok {
		return nil, false
	}
	return fieldVarOf(fa), true
}

// fieldVarOf returns the *types.Var the FieldAddr addresses — the field of the (possibly
// pointer-to) struct type. go/types interns a named type's fields, so the same field of the
// same named type is the same *types.Var across the program, which is what makes field
// identity a sound key for the store walk.
func fieldVarOf(fa *ssa.FieldAddr) *types.Var {
	t := fa.X.Type()
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem()
	}
	st, ok := t.Underlying().(*types.Struct)
	if !ok || fa.Field < 0 || fa.Field >= st.NumFields() {
		return nil
	}
	return st.Field(fa.Field)
}

// resolveField resolves the complete element set of every middleware slice stored into
// field fv, anywhere in the program. It visits every address of fv from fieldAddrsOf — an
// index built from a COMPLETE ssautil.AllFunctions sweep (pointer-receiver methods, wrappers,
// nested closures — per CLAUDE.md "collect functions completely"): under-collecting a store
// would under-approximate the set and could clear a seam that hides a real middleware, a
// false PROVEN. Every reference to the field's address must be a load or a store of a provable
// slice; any other use (the address escaping into a call, a store of an unprovable slice)
// makes the whole field unprovable (ok=false). The union over all stores over-approximates
// conditional writes, which only costs precision.
func (r *mwReclaimer) resolveField(fv *types.Var) fieldSet {
	if fv == nil {
		return fieldSet{ok: false}
	}
	if memo, hit := r.fieldMemo[fv]; hit {
		return memo
	}
	var funcs []*ssa.Function
	seen := map[*ssa.Function]bool{}
	add := func(fns []*ssa.Function) {
		for _, fn := range fns {
			if fn != nil && !seen[fn] {
				seen[fn] = true
				funcs = append(funcs, fn)
			}
		}
	}
	ok := true
	for _, fa := range r.fieldAddrsOf(fv) {
		if !ok {
			break
		}
		for _, ref := range referrers(fa) {
			switch x := ref.(type) {
			case *ssa.UnOp:
				if x.Op != token.MUL || !sliceReadOnly(x, fv, map[ssa.Value]bool{}) {
					ok = false
				}
			case *ssa.Store:
				if x.Addr != ssa.Value(fa) {
					ok = false
					continue
				}
				elems, eok := r.sliceElems(x.Val, fv)
				if !eok {
					ok = false
					continue
				}
				add(elems)
			default:
				// The field's address is used some other way (passed to a call, its
				// element address taken for a write): it can be mutated past what this
				// walk sees, so the set is not provable.
				ok = false
			}
		}
	}
	res := fieldSet{funcs: funcs, ok: ok}
	if !ok {
		res.funcs = nil
	}
	// ssautil.AllFunctions ranges a map, so the union order is run-dependent; sort on the
	// intrinsic FQN so the recovered middleware-edge set is byte-identical across runs
	// (the prime directive — determinism). The SET is already order-independent (a union).
	sortFuncsByFQNThenPos(res.funcs)
	r.fieldMemo[fv] = res
	return res
}

// fieldAddrsOf returns every *ssa.FieldAddr in the program that addresses field fv. The index
// is built lazily on first use from ONE ssautil.AllFunctions sweep and shared across the pass,
// so a service with many middleware-backed struct types pays a single whole-program scan
// rather than one per field (the union of stores resolveField needs is order-insensitive, so
// the map-iteration order of the build does not reach output — determinism is preserved).
func (r *mwReclaimer) fieldAddrsOf(fv *types.Var) []*ssa.FieldAddr {
	if r.fieldAddrs == nil {
		r.fieldAddrs = map[*types.Var][]*ssa.FieldAddr{}
		for fn := range ssautil.AllFunctions(r.prog) {
			for _, b := range fn.Blocks {
				for _, instr := range b.Instrs {
					if fa, ok := instr.(*ssa.FieldAddr); ok {
						if v := fieldVarOf(fa); v != nil {
							r.fieldAddrs[v] = append(r.fieldAddrs[v], fa)
						}
					}
				}
			}
		}
	}
	return r.fieldAddrs[fv]
}

// sliceReadOnly reports whether a field-slice value v (a load of field fv, or a value derived
// from one by re-slicing or appending) is used ONLY in ways that cannot write into fv's backing
// array beyond what the field-store walk already sees — the soundness guard that keeps the
// resolved element set complete. A slice that ESCAPES into a write (`s[i] = x`), into a
// non-builtin call, or into a store anywhere but fv itself could swap a middleware element past
// the walk, so the field becomes unprovable (the conservative direction — abstain).
//
// Recognized read-only uses:
//   - len/cap — header reads.
//   - append — reads the base/varargs; its RESULT may ALIAS fv's backing array (spare
//     capacity), so the result is recursed: it may be stored back into fv (the append-chain,
//     enumerated by the store walk via appendElems) or read, but a write THROUGH the result
//     (`tmp[i] = x`) is caught and abstains. Without this recursion an in-place append-then-
//     index-write would swap an element invisibly — a false PROVEN.
//   - iteration — `len` + an IndexAddr whose only uses are loads (the range read, and the
//     middleware loop's own element read).
//   - a re-slice — recursively read-only.
//   - a store of the slice back into fv itself — the field's own write, which the store walk
//     already enumerates; a store into ANY OTHER field/cell aliases fv's backing past the walk
//     and abstains.
//
// Anything else — the slice passed to a non-builtin call, an element address taken for a
// write, a `range` (the map form) — is treated as an escape.
func sliceReadOnly(v ssa.Value, fv *types.Var, seen map[ssa.Value]bool) bool {
	if seen[v] {
		return true
	}
	seen[v] = true
	for _, ref := range referrers(v) {
		switch x := ref.(type) {
		case *ssa.Call:
			bi, ok := x.Common().Value.(*ssa.Builtin)
			if !ok || !isReadOnlyBuiltin(bi.Name()) {
				return false
			}
			// append returns a slice that may alias v's backing array; its result must itself
			// be read-only (len/cap return scalars, so they need no recursion).
			if bi.Name() == "append" && !sliceReadOnly(x, fv, seen) {
				return false
			}
		case *ssa.Store:
			// A store of this slice is read-only-equivalent ONLY when it writes the slice back
			// into fv itself — that write is enumerated by the field-store walk. A store into
			// any other field/cell publishes an alias of fv's backing array the walk does not
			// follow, so abstain.
			if x.Val != v || !isFieldAddrOf(x.Addr, fv) {
				return false
			}
		case *ssa.IndexAddr:
			// An element address: read-only only if every use of it is a load (no Store).
			for _, iaRef := range referrers(x) {
				if u, ok := iaRef.(*ssa.UnOp); !ok || u.Op != token.MUL {
					return false
				}
			}
		case *ssa.Slice:
			if !sliceReadOnly(x, fv, seen) {
				return false
			}
		case *ssa.Range:
			// range over a slice is len+IndexAddr in SSA; a *ssa.Range is the map form,
			// which a []MiddlewareFunc never reaches, so treat it as an escape.
			return false
		default:
			return false
		}
	}
	return true
}

// isFieldAddrOf reports whether addr is the address of field fv (`&x.<fv>`).
func isFieldAddrOf(addr ssa.Value, fv *types.Var) bool {
	fa, ok := addr.(*ssa.FieldAddr)
	return ok && fv != nil && fieldVarOf(fa) == fv
}

// isReadOnlyBuiltin reports whether a builtin call on a slice only READS its argument: len/cap
// read the header; append reads its base/varargs and returns a fresh slice (whose own uses
// sliceReadOnly recurses into, since it may alias the argument's backing array).
func isReadOnlyBuiltin(name string) bool {
	return name == "len" || name == "cap" || name == "append"
}

// sliceElems resolves a []MiddlewareFunc VALUE to its complete element set, or abstains
// (ok=false). It handles the slice-construction shapes go/ssa emits: a const nil (empty), a
// `slice` of a local array literal (`[]MiddlewareFunc{a, b}`), and an `append` chain. For an
// append, the base must itself resolve — a base that is a load of the SAME field fv is folded
// to nothing (the field's other stores already account for it; fv is nil for a local slice,
// where no such base occurs). Any other shape (a func value from a parameter, an opaque call
// result, a load of a different field) is unprovable.
func (r *mwReclaimer) sliceElems(v ssa.Value, fv *types.Var) ([]*ssa.Function, bool) {
	switch x := v.(type) {
	case *ssa.Const:
		// The only constant slice value is nil — an empty set.
		return nil, x.IsNil()
	case *ssa.Slice:
		return r.sliceElems(x.X, fv)
	case *ssa.Alloc:
		return arrayAllocElems(x)
	case *ssa.Call:
		return r.appendElems(x, fv)
	}
	// A copy from ANOTHER struct field (`w.field = options.Middlewares`, the oapi-codegen
	// `HandlerWithOptions` bootstrap): the copied field's complete element set IS this store's
	// contribution, resolved transitively by the same program-wide store walk. A copy from the
	// SAME field (an identity store) folds to nothing — the field's other stores account for it.
	if g, ok := fieldCopyVar(v); ok {
		if g == fv {
			return nil, true
		}
		fs := r.fieldStoreSet(g)
		return fs.funcs, fs.ok
	}
	return nil, false
}

// fieldCopyVar returns the struct field a slice value is a copy of — a load of a field of a
// pointer/alloc (`*ssa.UnOp(MUL)` of `*ssa.FieldAddr`) or an extraction from a struct value
// (`*ssa.Field`) — and whether v is such a field access.
func fieldCopyVar(v ssa.Value) (*types.Var, bool) {
	switch x := v.(type) {
	case *ssa.UnOp:
		if x.Op == token.MUL {
			if fa, ok := x.X.(*ssa.FieldAddr); ok {
				if g := fieldVarOf(fa); g != nil {
					return g, true
				}
			}
		}
	case *ssa.Field:
		if st, ok := x.X.Type().Underlying().(*types.Struct); ok && x.Field >= 0 && x.Field < st.NumFields() {
			return st.Field(x.Field), true
		}
	}
	return nil, false
}

// fieldStoreSet resolves field G's complete element set from its STORES alone, program-wide —
// the transitive emptiness/element proof a field-copy store recurses into. Unlike resolveField
// it does NOT apply the loaded-value read-only guard: a field copied OUT (its load stored into
// another field, e.g. `w.HandlerMiddlewares = options.Middlewares`) cannot make G non-empty,
// because a field never SET to a non-empty value is nil at runtime and a nil slice cannot be
// element-mutated, so no alias of it can introduce an element. The ADDRESS-escape guard stays:
// if &G.field is used as anything but a load or a traced store, a store could happen past the
// walk, so abstain. Memoised and cycle-guarded (a field-copy cycle abstains — never a false
// PROVEN).
func (r *mwReclaimer) fieldStoreSet(fv *types.Var) fieldSet {
	if fv == nil {
		return fieldSet{ok: false}
	}
	if memo, hit := r.storeMemo[fv]; hit {
		return memo
	}
	if r.resolving[fv] {
		return fieldSet{ok: false}
	}
	r.resolving[fv] = true

	var funcs []*ssa.Function
	seen := map[*ssa.Function]bool{}
	ok := true
	for _, fa := range r.fieldAddrsOf(fv) {
		if !ok {
			break
		}
		for _, ref := range referrers(fa) {
			switch x := ref.(type) {
			case *ssa.UnOp:
				if x.Op != token.MUL {
					ok = false // taking the field's address some other way
				}
				// a load (copy-out / read) cannot make an unset field non-empty: allowed.
			case *ssa.Store:
				if x.Addr != ssa.Value(fa) {
					ok = false
					continue
				}
				elems, eok := r.sliceElems(x.Val, fv)
				if !eok {
					ok = false
					continue
				}
				for _, fn := range elems {
					if fn != nil && !seen[fn] {
						seen[fn] = true
						funcs = append(funcs, fn)
					}
				}
			default:
				ok = false // the field's address escapes — a store could happen past the walk
			}
		}
	}
	res := fieldSet{funcs: funcs, ok: ok}
	if !ok {
		res.funcs = nil
	}
	sortFuncsByFQNThenPos(res.funcs)
	delete(r.resolving, fv)
	r.storeMemo[fv] = res
	return res
}

// arrayAllocElems resolves the elements of a local array allocation backing a slice literal
// (`new [N]MiddlewareFunc`). Every IndexAddr into it must have a single constant-indexed
// store of a known func value; a non-constant index or an unresolvable element abstains.
func arrayAllocElems(alloc *ssa.Alloc) ([]*ssa.Function, bool) {
	if _, ok := alloc.Type().(*types.Pointer); !ok {
		return nil, false
	}
	if _, ok := alloc.Type().(*types.Pointer).Elem().Underlying().(*types.Array); !ok {
		return nil, false
	}
	var funcs []*ssa.Function
	for _, ref := range referrers(alloc) {
		switch x := ref.(type) {
		case *ssa.IndexAddr:
			if _, ok := x.Index.(*ssa.Const); !ok {
				return nil, false // dynamic index into the literal: not statically enumerable
			}
			stored := false
			for _, iaRef := range referrers(x) {
				st, ok := iaRef.(*ssa.Store)
				if !ok || st.Addr != ssa.Value(x) {
					continue
				}
				fn := handlerTarget(st.Val)
				if fn == nil {
					return nil, false
				}
				funcs = append(funcs, fn)
				stored = true
			}
			if !stored {
				return nil, false
			}
		case *ssa.Slice:
			// the `slice arr[:]` that turns the array into the slice value — ignore
		default:
			return nil, false // any other use of the array backing: not provable
		}
	}
	return funcs, true
}

// appendElems resolves the elements contributed by an `append(base, elems...)` call. The
// base must resolve (a same-field load folds to nothing; nil/empty or another provable slice
// is traced); the appended varargs slice is traced for its elements. A non-append call, or an
// unprovable base/element, abstains.
func (r *mwReclaimer) appendElems(call *ssa.Call, fv *types.Var) ([]*ssa.Function, bool) {
	c := call.Common()
	bi, ok := c.Value.(*ssa.Builtin)
	if !ok || bi.Name() != "append" || len(c.Args) != 2 {
		return nil, false
	}
	var funcs []*ssa.Function
	// base
	if fv != nil && isFieldLoad(c.Args[0], fv) {
		// the field's prior contents — accounted for by the field's other stores
	} else {
		base, ok := r.sliceElems(c.Args[0], fv)
		if !ok {
			return nil, false
		}
		funcs = append(funcs, base...)
	}
	// appended elements (the spread varargs slice)
	extra, ok := r.sliceElems(c.Args[1], fv)
	if !ok {
		return nil, false
	}
	funcs = append(funcs, extra...)
	return funcs, true
}

// isFieldLoad reports whether v is a load of field fv (`*ssa.UnOp(MUL)` of a FieldAddr on fv).
func isFieldLoad(v ssa.Value, fv *types.Var) bool {
	load, ok := v.(*ssa.UnOp)
	if !ok || load.Op != token.MUL {
		return false
	}
	fa, ok := load.X.(*ssa.FieldAddr)
	return ok && fieldVarOf(fa) == fv
}

// handlerTarget returns the concrete function a func/handler value wraps — a bare
// *ssa.Function, the func behind a MakeClosure, or either reached through the type
// conversions a func/handler value carries (ChangeType to MiddlewareFunc, the
// http.HandlerFunc Convert, MakeInterface to http.Handler). Returns nil for a value whose
// target is not statically a single function (a parameter, a load, a phi of several).
func handlerTarget(v ssa.Value) *ssa.Function {
	switch x := v.(type) {
	case *ssa.Function:
		return x
	case *ssa.MakeClosure:
		fn, _ := x.Fn.(*ssa.Function)
		return fn
	case *ssa.ChangeType:
		return handlerTarget(x.X)
	case *ssa.Convert:
		return handlerTarget(x.X)
	case *ssa.MakeInterface:
		return handlerTarget(x.X)
	}
	return nil
}
