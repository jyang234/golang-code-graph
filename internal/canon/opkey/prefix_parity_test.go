package opkey_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/canon/opkey"
)

// TestNoHardcodedOpKeyPrefix retires the op-key-prefix drift class as a whole:
// every consumer that builds or strips a canonical op-key prefix must use the
// opkey constant, never re-type the literal. This is the generalized guard
// behind the gate.go/diff.go DBPrefix fix — a single test so a NEW hardcoded
// prefix in ANY package fails CI instead of waiting for a review to spot it
// (CLAUDE.md: a rule applied in two places lives in one constant, guarded by a
// test).
//
// It scans the repo's non-test Go source for the exact standalone string literal
// of each prefix (e.g. "DB "). The opkey package itself defines them and is
// skipped; the byte-exact match (opening quote + value + closing quote) does not
// flag a full key like "HTTP GET /x" or the graph's "boundary:bus PUBLISH "
// label grammar, only a re-typed bare prefix.
func TestNoHardcodedOpKeyPrefix(t *testing.T) {
	prefixes := map[string]string{
		"HTTPPrefix":    opkey.HTTPPrefix,
		"DBPrefix":      opkey.DBPrefix,
		"RPCPrefix":     opkey.RPCPrefix,
		"PublishPrefix": opkey.PublishPrefix,
		"ConsumePrefix": opkey.ConsumePrefix,
		"SettlePrefix":  opkey.SettlePrefix,
	}

	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	opkeyDir := filepath.Dir(thisFile) // the constants' home — allowed to type the literals

	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip the opkey package, test fixtures, and VCS.
			if path == opkeyDir || strings.Contains(path, string(filepath.Separator)+"testdata") || strings.HasSuffix(path, "/.git") {
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
			lit := strconv.Quote(val) // exact Go string literal, e.g. "DB "
			if strings.Contains(string(src), lit) {
				rel, _ := filepath.Rel(repoRoot, path)
				t.Errorf("%s hardcodes the op-key prefix literal %s — use opkey.%s instead", rel, lit, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
