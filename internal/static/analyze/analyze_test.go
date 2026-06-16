package analyze_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/static/analyze"
)

// TestServiceNameFallsBackToModule checks that, with no `service:` in config, the
// service name is the module path's last segment, and that a missing .flowmap.yaml
// is tolerated (defaults apply).
func TestServiceNameFallsBackToModule(t *testing.T) {
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/widgets\n\ngo 1.24\n")
	writeFile(t, dir, "main.go", "package main\nfunc main() {}\n")
	// Deliberately no .flowmap.yaml.

	res, err := analyze.Analyze(dir)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if got := res.ServiceName(); got != "widgets" {
		t.Errorf("ServiceName = %q, want widgets (module last segment)", got)
	}
	if res.Config == nil {
		t.Error("missing config should yield a default, not nil")
	}
}

// TestServiceNameFromConfig pins the config-takes-precedence branch of
// ServiceName: an explicit `service:` overrides the module-last-segment fallback.
func TestServiceNameFromConfig(t *testing.T) {
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/widgets\n\ngo 1.24\n")
	writeFile(t, dir, "main.go", "package main\nfunc main() {}\n")
	writeFile(t, dir, ".flowmap.yaml", "service: billing\n")

	res, err := analyze.Analyze(dir)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if got := res.ServiceName(); got != "billing" {
		t.Errorf("ServiceName = %q, want billing (from config, overriding module)", got)
	}
}

// TestRegistrarsFromConfig checks bus-consumer hints become consumer registrars.
func TestRegistrarsFromConfig(t *testing.T) {
	cfg := mustConfig(t, "classify:\n  busConsume: [\"x/bus#Subscribe\"]\n")
	regs := analyze.Registrars(cfg)
	var found bool
	for _, r := range regs {
		if r.PkgPath == "x/bus" && r.Name == "Subscribe" {
			found = true
		}
	}
	if !found {
		t.Errorf("busConsume hint did not produce a consumer registrar: %+v", regs)
	}
}

func mustConfig(t *testing.T, y string) *config.Config {
	t.Helper()
	c, err := config.Load([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
