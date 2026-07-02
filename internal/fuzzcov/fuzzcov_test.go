// Package fuzzcov holds a single self-checking parity test: the nightly fuzz
// workflow must enumerate EVERY fuzz target in the repo. It lives in its own
// package so it can walk the whole tree without importing any of the fuzzed
// packages.
package fuzzcov

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// repoRoot resolves the module root from this test file's location, so the walk is
// independent of the caller's working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

var fuzzFuncRe = regexp.MustCompile(`(?m)^func (Fuzz\w+)\(`)

// TestFuzzWorkflowListsEveryTarget pins M-21: the nightly fuzz job (fuzz.yml) must
// list every FuzzXxx target defined in the repo. The PR gate only replays each
// target's seed corpus; only this workflow explores past the seeds, so a target
// omitted from the matrix is a target that never gets real fuzzing. Failing the
// build here keeps the matrix honest — a new fuzz target cannot be added without a
// matching workflow row.
func TestFuzzWorkflowListsEveryTarget(t *testing.T) {
	root := repoRoot(t)

	// Collect every fuzz target defined anywhere in the tree.
	targets := map[string]string{} // name -> file (for diagnostics)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip vendored, VCS, and fixture-module trees — fixtures are separate
			// modules and any Fuzz there is not part of this module's nightly job.
			switch d.Name() {
			case ".git", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range fuzzFuncRe.FindAllStringSubmatch(string(b), -1) {
			targets[m[1]] = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(targets) == 0 {
		t.Fatal("found no fuzz targets — the walk is broken")
	}

	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "fuzz.yml"))
	if err != nil {
		t.Fatalf("read fuzz.yml: %v", err)
	}
	listed := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)target:\s*(Fuzz\w+)`).FindAllStringSubmatch(string(wf), -1) {
		listed[m[1]] = true
	}

	for name, file := range targets {
		if !listed[name] {
			t.Errorf("fuzz target %s (%s) is not listed in .github/workflows/fuzz.yml — the nightly job would never explore it (M-21)", name, mustRel(root, file))
		}
	}
	// The reverse direction: a matrix row naming a target that no longer exists is
	// dead config that silently wastes a job slot.
	for name := range listed {
		if _, ok := targets[name]; !ok {
			t.Errorf("fuzz.yml lists target %s which is not defined anywhere in the repo — stale matrix row", name)
		}
	}
}

func mustRel(root, file string) string {
	if r, err := filepath.Rel(root, file); err == nil {
		return r
	}
	return file
}
