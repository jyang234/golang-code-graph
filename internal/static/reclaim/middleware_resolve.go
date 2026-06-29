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
	return sliceElems(slice, nil)
}

// sliceFieldVar reports whether slice is loaded from a struct field
// (`*ssa.UnOp(MUL)` of `*ssa.FieldAddr`, optionally behind re-slices), returning the
// field's *types.Var so the program-wide store walk can key on field identity.
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
// field fv, anywhere in the program. It walks ssautil.AllFunctions (a COMPLETE function set
// — pointer-receiver methods, wrappers, nested closures — per CLAUDE.md "collect functions
// completely"): under-collecting a store would under-approximate the set and could clear a
// seam that hides a real middleware, a false PROVEN. Every reference to the field's address
// must be a load or a store of a provable slice; any other use (the address escaping into a
// call, a store of an unprovable slice) makes the whole field unprovable (ok=false). The
// union over all stores over-approximates conditional writes, which only costs precision.
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
	for fn := range ssautil.AllFunctions(r.prog) {
		if !ok {
			break
		}
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				fa, isFA := instr.(*ssa.FieldAddr)
				if !isFA || fieldVarOf(fa) != fv {
					continue
				}
				for _, ref := range referrers(fa) {
					switch x := ref.(type) {
					case *ssa.UnOp:
						if x.Op != token.MUL || !sliceReadOnly(x, map[ssa.Value]bool{}) {
							ok = false
						}
					case *ssa.Store:
						if x.Addr != ssa.Value(fa) {
							ok = false
							continue
						}
						elems, eok := sliceElems(x.Val, fv)
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
		}
	}
	res := fieldSet{funcs: funcs, ok: ok}
	if !ok {
		res.funcs = nil
	}
	// ssautil.AllFunctions ranges a map, so the union order is run-dependent; sort on the
	// intrinsic FQN so the recovered middleware-edge set is byte-identical across runs
	// (the prime directive — determinism). The SET is already order-independent (a union).
	sort.Slice(res.funcs, func(i, j int) bool {
		return res.funcs[i].RelString(nil) < res.funcs[j].RelString(nil)
	})
	r.fieldMemo[fv] = res
	return res
}

// sliceReadOnly reports whether a field-slice value v (a load, or a value derived from one
// by re-slicing or used as an append base) is used ONLY in ways that cannot write into its
// backing array beyond what the field-store walk already sees — the soundness guard that
// keeps the resolved element set complete. A slice that ESCAPES into a write (`s[i] = x`),
// or into any call that is not a pure read of the header, could swap a middleware element
// past the walk, so the field becomes unprovable (the conservative direction — abstain).
//
// Recognized read-only uses: len/cap (header reads), append (reads the base/varargs; its
// result is a fresh value whose own writes are tracked when IT is stored to the field),
// iteration (`len` + an IndexAddr whose only uses are loads — the range read, and the
// middleware loop's own element read), and a re-slice (recursively read-only). Anything else
// — the slice passed to a non-builtin call, stored into another cell, an element address
// taken for a write — is treated as an escape.
func sliceReadOnly(v ssa.Value, seen map[ssa.Value]bool) bool {
	if seen[v] {
		return true
	}
	seen[v] = true
	for _, ref := range referrers(v) {
		switch x := ref.(type) {
		case *ssa.Call:
			if bi, ok := x.Common().Value.(*ssa.Builtin); !ok || !isReadOnlyBuiltin(bi.Name()) {
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
			if !sliceReadOnly(x, seen) {
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

// isReadOnlyBuiltin reports whether a builtin call on a slice only READS it (it cannot swap
// an element past the field-store walk): len/cap read the header; append reads its arguments
// and returns a fresh slice whose own stores are tracked when it is stored to the field.
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
func sliceElems(v ssa.Value, fv *types.Var) ([]*ssa.Function, bool) {
	switch x := v.(type) {
	case *ssa.Const:
		// The only constant slice value is nil — an empty set.
		return nil, x.IsNil()
	case *ssa.Slice:
		return sliceElems(x.X, fv)
	case *ssa.Alloc:
		return arrayAllocElems(x)
	case *ssa.Call:
		return appendElems(x, fv)
	}
	return nil, false
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
func appendElems(call *ssa.Call, fv *types.Var) ([]*ssa.Function, bool) {
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
		base, ok := sliceElems(c.Args[0], fv)
		if !ok {
			return nil, false
		}
		funcs = append(funcs, base...)
	}
	// appended elements (the spread varargs slice)
	extra, ok := sliceElems(c.Args[1], fv)
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
