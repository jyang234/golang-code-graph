// Package ciparity holds self-checking parity guards between the Makefile (the
// local gate) and the CI workflow (the remote gate). CLAUDE.md's trust boundary
// requires "CI mirrors make verify exactly"; a pin that lives in two files as prose
// drifts silently. These tests turn that prose into an enforced invariant. The
// package imports nothing from the toolchain so it can read the raw config files.
package ciparity

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

// repoRoot resolves the module root from this test file's location.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// TestGolangciLintVersionParity pins R-6: the golangci-lint version the Makefile
// pins (GOLANGCI_LINT_VERSION) and the version CI installs (the `@vX.Y.Z` on the
// `go install .../golangci-lint@...` line in gates.yml) must be byte-identical, so
// `make lint` and the CI lint step run the SAME linter build. The two were kept in
// step only by a comment on each side; this guard makes the invariant self-checking
// (CLAUDE.md: parity needs a guard, not just prose).
func TestGolangciLintVersionParity(t *testing.T) {
	root := repoRoot(t)

	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	mkMatch := regexp.MustCompile(`(?m)^GOLANGCI_LINT_VERSION\s*\?=\s*(v[0-9]+\.[0-9]+\.[0-9]+)`).FindSubmatch(mk)
	if mkMatch == nil {
		t.Fatal("Makefile: could not find GOLANGCI_LINT_VERSION pin")
	}
	mkVer := string(mkMatch[1])

	ci, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "gates.yml"))
	if err != nil {
		t.Fatalf("read gates.yml: %v", err)
	}
	ciMatch := regexp.MustCompile(`golangci-lint@(v[0-9]+\.[0-9]+\.[0-9]+)`).FindAllSubmatch(ci, -1)
	if len(ciMatch) == 0 {
		t.Fatal("gates.yml: could not find a golangci-lint@vX.Y.Z install pin")
	}
	for _, m := range ciMatch {
		if ciVer := string(m[1]); ciVer != mkVer {
			t.Errorf("golangci-lint version drift: Makefile pins %s but gates.yml installs %s — "+
				"`make lint` and CI would run different linter builds (R-6)", mkVer, ciVer)
		}
	}
}
