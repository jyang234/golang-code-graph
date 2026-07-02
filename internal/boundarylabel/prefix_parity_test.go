package boundarylabel_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/boundarylabel"
)

// TestNoHardcodedBoundaryPrefix retires the boundary-label-prefix drift class the
// same way TestNoHardcodedOpKeyPrefix retired the op-key one: every consumer that
// builds or strips a "boundary:db " / "boundary:bus " prefix must use the
// boundarylabel constant, never re-type the literal. Roughly ten packages (the
// graphio producer, the fitness proposers, impact, impeach, schemadrift, …) had
// re-typed these with no shared home; a drift in one silently mis-parses every
// DB/bus effect it touches. A single repo-scan so a NEW hardcoded prefix in ANY
// package fails CI instead of waiting for a review to spot it (CLAUDE.md: a rule
// applied in two places lives in one constant, guarded by a test).
//
// It scans non-test Go source for the exact standalone string literal of each
// prefix (opening quote + value + closing quote). The boundarylabel package itself
// defines them and is skipped; the byte-exact match does not flag a fuller label
// like "boundary:db INSERT users" or "boundary:bus PUBLISH orders", only a
// re-typed bare prefix.
func TestNoHardcodedBoundaryPrefix(t *testing.T) {
	prefixes := map[string]string{
		"DBPrefix":  boundarylabel.DBPrefix,
		"BusPrefix": boundarylabel.BusPrefix,
	}

	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	homeDir := filepath.Dir(thisFile) // the constants' home — allowed to type the literals

	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip the boundarylabel package, test fixtures, and VCS.
			if path == homeDir || strings.Contains(path, string(filepath.Separator)+"testdata") || strings.HasSuffix(path, "/.git") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for name, val := range prefixes {
			lit := strconv.Quote(val) // exact Go string literal, e.g. "boundary:db "
			if strings.Contains(string(src), lit) {
				rel, _ := filepath.Rel(repoRoot, path)
				t.Errorf("%s hardcodes the boundary-label prefix literal %s — use boundarylabel.%s instead", rel, lit, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
