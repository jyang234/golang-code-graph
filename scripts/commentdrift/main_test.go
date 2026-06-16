package main

import (
	"strings"
	"testing"
)

func TestFindDrift(t *testing.T) {
	const path = "x.go"
	cases := []struct {
		name     string
		old, new string
		wantHit  bool
	}{
		{
			name:    "asserting comment, body changed, doc unchanged -> flag",
			old:     "package p\n// F always returns a sorted slice.\nfunc F() int { return 1 }\n",
			new:     "package p\n// F always returns a sorted slice.\nfunc F() int { return 2 }\n",
			wantHit: true,
		},
		{
			name:    "doc updated alongside body -> no flag",
			old:     "package p\n// F always returns 1.\nfunc F() int { return 1 }\n",
			new:     "package p\n// F always returns 2.\nfunc F() int { return 2 }\n",
			wantHit: false,
		},
		{
			name:    "prose comment (no checkable claim) -> no flag",
			old:     "package p\n// F computes a thing for the caller.\nfunc F() int { return 1 }\n",
			new:     "package p\n// F computes a thing for the caller.\nfunc F() int { return 2 }\n",
			wantHit: false,
		},
		{
			name:    "body unchanged -> no flag",
			old:     "package p\n// F never panics.\nfunc F() int { return 1 }\n",
			new:     "package p\n// F never panics.\nfunc F() int { return 1 }\n",
			wantHit: false,
		},
		{
			name:    "reformatting only is not a body change -> no flag",
			old:     "package p\n// F must return a stable value.\nfunc F() int { return 1 }\n",
			new:     "package p\n// F must return a stable value.\nfunc F() int {\n\treturn 1\n}\n",
			wantHit: false,
		},
		{
			name:    "newly added function -> no flag",
			old:     "package p\n",
			new:     "package p\n// F is always sorted.\nfunc F() int { return 1 }\n",
			wantHit: false,
		},
		{
			name:    "method keyed by receiver -> flag",
			old:     "package p\ntype T struct{}\n// M must stay idempotent.\nfunc (t *T) M() int { return 1 }\n",
			new:     "package p\ntype T struct{}\n// M must stay idempotent.\nfunc (t *T) M() int { return 9 }\n",
			wantHit: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findDrift(path, tc.old, tc.new)
			if hit := len(got) > 0; hit != tc.wantHit {
				t.Fatalf("findDrift hit=%v, want %v (findings: %v)", hit, tc.wantHit, got)
			}
		})
	}
}

func TestMakesClaim(t *testing.T) {
	if !makesClaim("the result is always sorted") {
		t.Error("expected an asserting comment to be a claim")
	}
	if makesClaim("a helper for the caller") {
		t.Error("prose should not register as a claim")
	}
}

func TestFnKeyDistinguishesMethods(t *testing.T) {
	src := "package p\ntype T struct{}\nfunc Free() {}\nfunc (t *T) M() {}\nfunc (t T) N() {}\n"
	fns := parseFns("x.go", src)
	for _, want := range []string{"Free", "*T.M", "T.N"} {
		if _, ok := fns[want]; !ok {
			t.Errorf("missing key %q (got %v)", want, keys(fns))
		}
	}
}

func keys(m map[string]fnInfo) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return strings.Join(ks, ",")
}
