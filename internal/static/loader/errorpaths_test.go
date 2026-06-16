package loader_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/loader"
)

// writeModule lays out a hermetic single-module service under a temp dir and
// returns its path. GOWORK=off keeps the repo's go.work out of the picture so the
// fixture loads in isolation. files maps a path relative to the module root to its
// contents; a go.mod is always written first.
func writeModule(t *testing.T, modPath string, files map[string]string) string {
	t.Helper()
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	write(t, dir, "go.mod", "module "+modPath+"\n\ngo 1.24\n")
	for rel, content := range files {
		write(t, dir, rel, content)
	}
	return dir
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLoadTypeCheckErrorFailsLoudly is the central front-end contract: a unit that
// fails to type-check must abort the load, NOT return a silently-truncated package
// set that the graph is then built from. A subtle regression here would let every
// downstream verdict reason over a partial program.
func TestLoadTypeCheckErrorFailsLoudly(t *testing.T) {
	dir := writeModule(t, "example.com/broken", map[string]string{
		"main.go": "package main\n\nfunc main() {\n\tvar x int = \"not an int\"\n\t_ = x\n}\n",
	})
	svc, err := loader.Load(dir)
	if err == nil {
		t.Fatalf("Load of a type-incorrect unit must fail; got service with %d packages", len(svc.Packages))
	}
	// The summary must name the failing package and the load layer.
	if !strings.Contains(err.Error(), "loader:") {
		t.Errorf("error not attributed to the loader: %v", err)
	}
}

// TestLoadBrokenDependencyFailsFromClosure pins that collectErrors walks the whole
// import closure, not just the roots: a root package that itself type-checks but
// imports a broken first-party sibling must still abort, because SSA over a broken
// dependency would be corrupt.
func TestLoadBrokenDependencyFailsFromClosure(t *testing.T) {
	dir := writeModule(t, "example.com/dep", map[string]string{
		// Root package is well-formed and imports the broken sibling.
		"main.go": "package main\n\nimport \"example.com/dep/internal/bad\"\n\nfunc main() { bad.F() }\n",
		// Sibling fails to type-check.
		"internal/bad/bad.go": "package bad\n\nfunc F() { var y string = 42; _ = y }\n",
	})
	if _, err := loader.Load(dir); err == nil {
		t.Fatal("Load must fail when a first-party dependency does not type-check")
	}
}

// TestLoadErrorSummaryTruncates pins collectErrors' deterministic summary: with
// more than the cap of distinct errors it sorts them and appends a "(+N more)"
// suffix instead of dumping an unbounded, order-unstable wall of text.
func TestLoadErrorSummaryTruncates(t *testing.T) {
	// Each line is its own undefined-identifier error; well past the cap of 10.
	var b strings.Builder
	b.WriteString("package main\n\nfunc main() {\n")
	for i := 0; i < 20; i++ {
		b.WriteString("\t_ = undefinedSymbol\n")
	}
	b.WriteString("}\n")
	dir := writeModule(t, "example.com/many", map[string]string{"main.go": b.String()})

	_, err := loader.Load(dir)
	if err == nil {
		t.Fatal("Load of a unit with many errors must fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "error(s)") {
		t.Errorf("summary missing error count: %v", msg)
	}
	if !strings.Contains(msg, "(+") || !strings.Contains(msg, "more)") {
		t.Errorf("summary did not truncate with a (+N more) suffix: %v", msg)
	}
}

// TestLoadTestOnlyDirFails pins the "no production source" guard: a module whose
// only Go files are *_test.go loads as a package with no compiled files and must
// be rejected rather than analyzed as an empty unit.
func TestLoadTestOnlyDirFails(t *testing.T) {
	dir := writeModule(t, "example.com/testonly", map[string]string{
		"only_test.go": "package main\n\nimport \"testing\"\n\nfunc TestNothing(t *testing.T) {}\n",
	})
	_, err := loader.Load(dir)
	if err == nil {
		t.Fatal("Load of a test-only module must fail")
	}
	if !strings.Contains(err.Error(), "no packages with Go source") {
		t.Errorf("unexpected error for test-only module: %v", err)
	}
}

// TestLoadEmptyDirFails pins the empty-unit guard for a directory with a go.mod
// but no Go packages at all.
func TestLoadEmptyDirFails(t *testing.T) {
	dir := writeModule(t, "example.com/empty", nil)
	if _, err := loader.Load(dir); err == nil {
		t.Fatal("Load of a module with no packages must fail")
	}
}

// TestLoadSelectsMainModule pins moduleOf's primary path: when the toolchain marks
// a main module, that module is the unit's module and its path is reported.
func TestLoadSelectsMainModule(t *testing.T) {
	dir := writeModule(t, "example.com/widgets", map[string]string{
		"main.go":             "package main\n\nfunc main() {}\n",
		"internal/a/a.go":     "package a\n\nfunc A() {}\n",
		"internal/b/sub/b.go": "package sub\n\nfunc B() {}\n",
	})
	svc, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if svc.Module == nil {
		t.Fatal("expected a module, got nil")
	}
	if svc.Module.Path != "example.com/widgets" {
		t.Errorf("module path = %q, want example.com/widgets", svc.Module.Path)
	}
	if !svc.Module.Main {
		t.Errorf("expected the loaded module to be marked Main")
	}
}
