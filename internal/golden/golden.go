// Package golden is the snapshot-assertion lifecycle behind the behavioral gate
// (golden-diff spec, canon spec §6). It compares a freshly canonicalized trace to
// the committed golden IR and, under -update, rewrites both the golden JSON and
// the rendered Mermaid view. Equality ignores the Discards manifest — it records
// only what was dropped, for review transparency, and must never perturb the
// gate.
//
// The assertion is on the IR, not the Mermaid: the diagram is a committed view
// derived from the IR, so a renderer change can never cause a false golden.
package golden

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/render"
	"github.com/jyang234/golang-code-graph/ir"
)

// update is the standard Go golden-file flag: `go test -update` rebases the
// committed snapshots instead of asserting against them.
var update = flag.Bool("update", false, "rewrite golden snapshots and rendered views")

// Update reports whether -update was passed.
func Update() bool { return *update }

// Compare asserts got against the committed golden for name in dir, or rebases it
// when update is true. On a rebase it writes <stem>.golden.json (the gated IR)
// and <stem>.flow.md (the rendered view). On an assertion it returns a
// human-readable error describing the structural difference, or nil on a match.
func Compare(got *ir.CanonicalTrace, dir, name string, update bool) error {
	stem := filepath.Join(dir, Slug(name))
	goldenPath := stem + ".golden.json"
	mdPath := stem + ".flow.md"

	if update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		b, err := got.Marshal()
		if err != nil {
			return err
		}
		if err := os.WriteFile(goldenPath, b, 0o644); err != nil {
			return err
		}
		return os.WriteFile(mdPath, []byte(render.Mermaid(got)), 0o644)
	}

	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("golden: no snapshot at %s; run the test with -update to create it", goldenPath)
		}
		return err
	}
	want, err := ir.Load(raw)
	if err != nil {
		return fmt.Errorf("golden: %s is corrupt: %w", goldenPath, err)
	}
	gotBytes := canonicalBytes(got)
	wantBytes := canonicalBytes(want)
	if string(gotBytes) != string(wantBytes) {
		return fmt.Errorf("golden: %s does not match the observed flow (run -update to rebase if intended):\n%s",
			goldenPath, diff(string(wantBytes), string(gotBytes)))
	}
	return nil
}

// canonicalBytes serializes a trace with the Discards manifest zeroed, so
// equality rests on flow structure alone and never on the review-only record of
// what was dropped.
func canonicalBytes(t *ir.CanonicalTrace) []byte {
	cp := *t
	cp.Discards = ir.DiscardManifest{}
	b, _ := cp.Marshal()
	return b
}

// Slug turns a flow name into a stable file stem: a leading path or method is
// folded in, runs of non-alphanumeric characters become single underscores.
func Slug(name string) string {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if !prevUnderscore {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

// diff produces a compact line-oriented view of the first divergence between two
// canonical JSON documents — enough for a reviewer to see what changed. The
// prioritized, semantic change set is the diff engine's job (Phase 7); this is
// the readable fallback the gate prints on a raw mismatch.
func diff(want, got string) string {
	wl := strings.Split(strings.TrimRight(want, "\n"), "\n")
	gl := strings.Split(strings.TrimRight(got, "\n"), "\n")
	var b strings.Builder
	n := len(wl)
	if len(gl) > n {
		n = len(gl)
	}
	shown := 0
	for i := 0; i < n && shown < 40; i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w == g {
			continue
		}
		if i < len(wl) {
			b.WriteString("- " + w + "\n")
		}
		if i < len(gl) {
			b.WriteString("+ " + g + "\n")
		}
		shown++
	}
	if shown == 0 {
		b.WriteString("(documents differ only in line count or trailing whitespace)\n")
	}
	return b.String()
}
