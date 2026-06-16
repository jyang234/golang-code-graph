package signatures_test

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/jyang234/golang-code-graph/internal/static/signatures"
)

// buildInline compiles one inline source file into SSA and returns every function
// in the program (declared, instantiated, and synthetic wrappers alike). It gives
// the signature renderer's edge cases — variadics, unnamed results, generic
// instantiations, promoted-method wrappers — under test without perturbing the
// fixture goldens. Generics are instantiated so the renderer sees the shapes a
// real program produces.
func buildInline(t *testing.T, src string) *ssa.Program {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pkg := types.NewPackage("example.com/fix", "")
	spkg, _, err := ssautil.BuildPackage(
		&types.Config{Importer: importer.Default()}, fset, pkg, []*ast.File{f},
		ssa.SanityCheckFunctions|ssa.InstantiateGenerics)
	if err != nil {
		t.Fatalf("build SSA: %v", err)
	}
	return spkg.Prog
}

// findExact returns the built function whose fully-qualified name equals fqn, or
// nil. It never matches a closure of the named function.
func findExact(prog *ssa.Program, fqn string) *ssa.Function {
	for fn := range ssautil.AllFunctions(prog) {
		if fn.RelString(nil) == fqn {
			return fn
		}
	}
	return nil
}

// findContains returns any built function whose fully-qualified name contains
// substr, preferring the shortest match so a promoted-method wrapper is picked
// over an unrelated longer name.
func findContains(prog *ssa.Program, substr string) *ssa.Function {
	var best *ssa.Function
	for fn := range ssautil.AllFunctions(prog) {
		if !strings.Contains(fn.RelString(nil), substr) {
			continue
		}
		if best == nil || len(fn.RelString(nil)) < len(best.RelString(nil)) {
			best = fn
		}
	}
	return best
}

const edgeSrc = `package fix

import "io"

// Variadic with an unnamed result.
func Join(sep string, parts ...string) string { _ = sep; _ = parts; return "" }

// Multiple unnamed results.
func Pair() (int, error) { return 0, nil }

// Generic function over a constraint.
func First[T any](xs []T) (T, bool) { var z T; return z, len(xs) > 0 }

type Base struct{}

// Method on Base; promoted onto Outer below.
func (Base) Ping(w io.Writer) error { _ = w; return nil }

// Outer embeds Base, so Base.Ping is promoted onto *Outer as a synthetic wrapper.
type Outer struct{ Base }

// A pointer-receiver method, to exercise receiver rendering.
func (o *Outer) Pong() {}

// Force instantiation of the generic so the program contains First[int].
func use() { _, _ = First([]int{1}); var o Outer; _ = o; _ = (&o).Pong }
`

// TestVariadicAndUnnamedResults pins that a variadic parameter and an unnamed
// result render faithfully — the shapes most likely to be silently dropped.
func TestVariadicAndUnnamedResults(t *testing.T) {
	prog := buildInline(t, edgeSrc)

	join := findExact(prog, "example.com/fix.Join")
	if join == nil {
		t.Fatal("Join not found")
	}
	if sig := signatures.Of(join); !strings.Contains(sig, "...string") {
		t.Errorf("variadic param not rendered: %q", sig)
	}

	pair := findExact(prog, "example.com/fix.Pair")
	if pair == nil {
		t.Fatal("Pair not found")
	}
	// ObjectString renders unnamed results as a bare type tuple "(int, error)".
	if sig := signatures.Of(pair); !strings.Contains(sig, "int") || !strings.Contains(sig, "error") {
		t.Errorf("unnamed results not rendered: %q", sig)
	}
}

// TestGenericDeclarationAndInstantiation pins both the generic declaration form
// (type parameters present) and that an instantiation still renders a usable
// signature rather than panicking or emitting an empty string.
func TestGenericDeclarationAndInstantiation(t *testing.T) {
	prog := buildInline(t, edgeSrc)

	decl := findExact(prog, "example.com/fix.First")
	if decl == nil {
		t.Fatal("generic First not found")
	}
	if sig := signatures.Of(decl); !strings.Contains(sig, "[T any]") {
		t.Errorf("generic declaration omits type params: %q", sig)
	}

	// The instantiated function (First[int]) renders through the object form too;
	// it must produce a non-empty, type-bearing signature.
	inst := findContains(prog, "example.com/fix.First[")
	if inst == nil {
		t.Skip("no instantiation surfaced; declaration coverage already asserted")
	}
	if sig := signatures.Of(inst); sig == "" {
		t.Errorf("instantiated generic rendered empty signature")
	}
}

// TestPromotedMethodWrapper exercises the synthetic-function fallback: a method
// promoted through an embedded field is realized as a wrapper whose Object() is
// nil, so Of must fall back to TypeString of the bare signature (receiver +
// params) rather than emitting "".
func TestPromotedMethodWrapper(t *testing.T) {
	prog := buildInline(t, edgeSrc)
	// The promoted wrapper is named like "(example.com/fix.Outer).Ping" with a nil
	// Object; pick the Ping that has no type object (the wrapper), if present.
	var wrapper *ssa.Function
	for fn := range ssautil.AllFunctions(prog) {
		if strings.Contains(fn.RelString(nil), ".Ping") && fn.Object() == nil {
			wrapper = fn
			break
		}
	}
	if wrapper == nil {
		t.Skip("no synthetic promoted-method wrapper surfaced in this toolchain")
	}
	sig := signatures.Of(wrapper)
	if sig == "" {
		t.Fatalf("synthetic wrapper rendered empty signature")
	}
	if !strings.Contains(sig, "func") {
		t.Errorf("wrapper signature not a function type: %q", sig)
	}
}

// TestSyntheticFunctionFallbacks covers the two non-object branches of Of with
// hand-built SSA functions, deterministically and without depending on which
// wrappers a given toolchain synthesizes:
//   - a function with a Signature but no Object renders via TypeString;
//   - a function with neither renders as the empty string (the documented floor).
func TestSyntheticFunctionFallbacks(t *testing.T) {
	sig := types.NewSignatureType(nil, nil, nil, types.NewTuple(), types.NewTuple(), false)
	withSig := &ssa.Function{Signature: sig}
	if got := signatures.Of(withSig); got != "func()" {
		t.Errorf("objectless signature: Of = %q, want %q", got, "func()")
	}

	bare := &ssa.Function{}
	if got := signatures.Of(bare); got != "" {
		t.Errorf("signatureless function: Of = %q, want empty string", got)
	}
}
