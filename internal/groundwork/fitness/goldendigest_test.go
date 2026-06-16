package fitness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var updateDigests = flag.Bool("updatedigests", false,
	"rewrite testdata/groundwork/goldens/digests.json from the current goldens")

// TestGoldenContentDigest is the content-level sibling of TestGoldenSectionManifest.
// The count manifest catches a regen that changes a section's SIZE, but a
// same-count content swap — an edge retargeted, a node renamed, a label format
// shift — slides straight through it, because the graph goldens are test INPUTS
// (regen.sh rewrites them wholesale, the tests then read the new bytes). This pins
// a SHA-256 of every golden's bytes in digests.json, a file regen.sh never
// rewrites: any content change fails this test until a human regenerates the
// digests (`go test ./internal/groundwork/fitness -run TestGoldenContentDigest
// -updatedigests`) in the same commit — which puts the golden's content diff in
// front of a reviewer instead of nobody. It also covers the golden families the
// count manifest does not (contract, branch, artifact).
func TestGoldenContentDigest(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "testdata", "groundwork", "goldens")
	got, err := goldenDigests(dir)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "digests.json")
	if *updateDigests {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d goldens)", path, len(got))
		return
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read digests: %v (run with -updatedigests to create it)", err)
	}
	var want map[string]string
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatalf("decode digests: %v", err)
	}

	for name, sum := range got {
		switch w, ok := want[name]; {
		case !ok:
			t.Errorf("golden %s has no digest entry — run -updatedigests and review its content in the same commit", name)
		case w != sum:
			t.Errorf("golden %s content changed (digest %s != pinned %s) — if intended, run -updatedigests and review the diff; if not, a regression just tried to launder itself", name, sum, w)
		}
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("digests.json pins %s but no such golden exists — drop the stale entry", name)
		}
	}
}

// goldenDigests returns the SHA-256 (hex) of every committed golden in dir,
// excluding the two ratchet files themselves.
func goldenDigests(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		if !strings.HasSuffix(n, ".json") || n == "manifest.json" || n == "digests.json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		out[n] = hex.EncodeToString(sum[:])
	}
	return out, nil
}
