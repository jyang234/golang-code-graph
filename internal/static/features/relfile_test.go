package features_test

import (
	"path/filepath"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/features"
)

// TestRelFile pins the service-relative path predicate shared by obligations' site
// strings and the graph's node File field (CLAUDE.md one source of truth — the parity
// the comment on features.RelFile names is enforced here, not just asserted). The three
// rungs: a file inside baseDir resolves to the slash-separated relative path (the only
// form independent of where the repo is checked out); a file outside (above) baseDir
// falls to the portable "<pkgPath>/<base>" form; and an empty baseDir — no real caller's
// mode — also takes the portable form rather than emitting a bare, collision-prone base
// name.
func TestRelFile(t *testing.T) {
	base := filepath.Join("/home", "u", "svc")
	pkg := "example.com/svc/internal/store"
	tests := []struct {
		name     string
		filename string
		baseDir  string
		want     string
	}{
		{
			name:     "inside baseDir → relative slash path",
			filename: filepath.Join(base, "internal", "store", "store.go"),
			baseDir:  base,
			want:     "internal/store/store.go",
		},
		{
			name:     "at baseDir root",
			filename: filepath.Join(base, "main.go"),
			baseDir:  base,
			want:     "main.go",
		},
		{
			name:     "above baseDir (../) → portable package-qualified form",
			filename: filepath.Join("/home", "u", "other", "gen.go"),
			baseDir:  base,
			want:     pkg + "/gen.go",
		},
		{
			name:     "empty baseDir → portable form, never a bare base name",
			filename: filepath.Join(base, "main.go"),
			baseDir:  "",
			want:     pkg + "/main.go",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := features.RelFile(tc.filename, tc.baseDir, pkg); got != tc.want {
				t.Errorf("RelFile(%q, %q, %q) = %q, want %q", tc.filename, tc.baseDir, pkg, got, tc.want)
			}
		})
	}
}

// TestRelFileIsCheckoutIndependent is the determinism claim the node File field and the
// obligation site strings both rest on: the SAME file at the SAME position within two
// repos checked out at DIFFERENT absolute locations yields byte-identical output. This
// is what lets the graph be a hard CI gate input (the relative path is a pure function
// of the in-repo layout, not the checkout dir).
func TestRelFileIsCheckoutIndependent(t *testing.T) {
	pkg := "example.com/svc/pkg"
	rel := filepath.Join("internal", "handler", "h.go")
	a := features.RelFile(filepath.Join("/home", "alice", "svc", rel), filepath.Join("/home", "alice", "svc"), pkg)
	b := features.RelFile(filepath.Join("/ci", "build", "1234", "svc", rel), filepath.Join("/ci", "build", "1234", "svc"), pkg)
	if a != b {
		t.Errorf("RelFile is checkout-dependent: %q != %q", a, b)
	}
	if a != "internal/handler/h.go" {
		t.Errorf("RelFile = %q, want the in-repo relative path", a)
	}
}
