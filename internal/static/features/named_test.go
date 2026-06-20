package features_test

import (
	"go/token"
	"go/types"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/features"
)

// TestNamedTypeIs pins the shared nil-safe defined-type identity compare (one source of
// truth with sqlfold.namedIs and the blind-spot benign-func tier): an exact (pkg, name)
// match is true; a name, package, nil-named, or nil-package case is false and never
// panics.
func TestNamedTypeIs(t *testing.T) {
	mk := func(p *types.Package, name string) *types.Named {
		obj := types.NewTypeName(token.NoPos, p, name, nil)
		return types.NewNamed(obj, types.Typ[types.Int], nil)
	}
	ctx := types.NewPackage("context", "context")
	cancel := mk(ctx, "CancelFunc")

	if !features.NamedTypeIs(cancel, "context", "CancelFunc") {
		t.Error("exact (pkg, name) match should be true")
	}
	if features.NamedTypeIs(cancel, "context", "Context") {
		t.Error("name mismatch must be false")
	}
	if features.NamedTypeIs(cancel, "time", "CancelFunc") {
		t.Error("package mismatch must be false")
	}
	if features.NamedTypeIs(nil, "context", "CancelFunc") {
		t.Error("a nil named (value was not a defined type) must be false")
	}
	if features.NamedTypeIs(mk(nil, "error"), "context", "CancelFunc") {
		t.Error("a named type with no package must be false and must not panic")
	}
}
