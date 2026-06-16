package policy

import "testing"

// MatchPrefix must bind at identifier boundaries only — a bare strings.HasPrefix
// would let a pattern split an identifier and bind an unrelated symbol, the
// fail-open that this matcher exists to prevent across every gate and ratchet.
func TestMatchPrefix(t *testing.T) {
	cases := []struct {
		s, prefix string
		want      bool
	}{
		{"example.com/svc/internal/app", "example.com/svc/internal/app", true},          // exact
		{"example.com/svc/internal/app/sub", "example.com/svc/internal/app", true},      // sub-package, '/' boundary
		{"example.com/svc/internal/application", "example.com/svc/internal/app", false}, // sibling — must NOT bind
		{"app.GetUserAvatar", "app.GetUser", false},                                     // distinct symbol, ident continues
		{"app.GetUser", "app.GetUser", true},                                            // exact symbol
		{"pkg.Fn", "pkg", true},                                                         // package binds its members, '.' boundary
		{"pkgs.Fn", "pkg", false},                                                       // sibling package
		{"db INSERT users_audit", "db INSERT users", false},                             // new table, '_' is an ident byte
		{"db INSERT users", "db INSERT", true},                                          // op-level family, ' ' boundary
		{"(*pkg.T).M", "(*pkg.T)", true},                                                // receiver type, '.' boundary
	}
	for _, c := range cases {
		if got := MatchPrefix(c.s, c.prefix); got != c.want {
			t.Errorf("MatchPrefix(%q, %q) = %v, want %v", c.s, c.prefix, got, c.want)
		}
	}
}

// LayerOf assigns a package to the layer whose declared prefix binds it at a
// boundary; a sibling whose name merely extends a declared package's name must
// fall through to no layer rather than be silently claimed.
func TestLayerOfBoundary(t *testing.T) {
	p := &Policy{Layers: []Layer{
		{Name: "app", Packages: []string{"example.com/svc/internal/app"}},
	}}
	if got := p.LayerOf("example.com/svc/internal/app/sub"); got != "app" {
		t.Errorf("sub-package: LayerOf = %q, want %q", got, "app")
	}
	if got := p.LayerOf("example.com/svc/internal/application"); got != "" {
		t.Errorf("sibling package must not be claimed by app layer: LayerOf = %q, want \"\"", got)
	}
}
