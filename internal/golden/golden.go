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

	"github.com/jyang234/golang-code-graph/internal/diff"
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
	if update {
		return WriteSnapshot(got, dir, Slug(name))
	}

	goldenPath := filepath.Join(dir, Slug(name)) + ".golden.json"
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
	gotBytes, err := canonicalBytes(got)
	if err != nil {
		return fmt.Errorf("golden: serializing the observed flow: %w", err)
	}
	wantBytes, err := canonicalBytes(want)
	if err != nil {
		return fmt.Errorf("golden: serializing %s: %w", goldenPath, err)
	}
	if string(gotBytes) != string(wantBytes) {
		return fmt.Errorf("golden: %s does not match the observed flow (run -update to rebase if intended):\n%s",
			goldenPath, changeSet(want, got, string(wantBytes), string(gotBytes)))
	}
	return nil
}

// WriteSnapshot rebases the committed golden for an already-slugged stem: it writes
// <dir>/<stem>.golden.json (the STAMPLESS gated IR) and <dir>/<stem>.flow.md (the
// rendered view). It is the SINGLE writer both the in-test golden lifecycle (Compare
// under -update) and the `flowmap behavior ingest --corpus-dir` impeach-corpus
// exporter go through, so a committed impeach corpus is byte-identical however it was
// produced — a harness -update re-drive or a from-collector ingest both land the same
// stampless trace that loadCommittedCorpus/ir.Load reads.
func WriteSnapshot(got *ir.CanonicalTrace, dir, stem string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, stem)
	// A committed golden is STAMPLESS — the code-identity Stamp is run-varying
	// provenance (the deployed commit), so writing it would churn the file each
	// deploy and stale-skew every later audit against it, exactly as the static
	// graph golden carries no --stamp. Identity is injected at audit time.
	cp := stampless(got)
	b, err := cp.Marshal()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".golden.json", b, 0o644); err != nil {
		return err
	}
	return os.WriteFile(path+".flow.md", []byte(render.Mermaid(got)), 0o644)
}

// changeSet renders the prioritized structural diff (the authoritative,
// reviewer-facing change set). It falls back to a line diff only when the
// structural diff is empty yet the bytes differ — an edit the differ does not
// model, such as a span's owning-service label.
func changeSet(want, got *ir.CanonicalTrace, wantBytes, gotBytes string) string {
	changes := diff.Diff(want, got)
	if len(changes) == 0 {
		return diffLines(wantBytes, gotBytes)
	}
	var b strings.Builder
	for _, c := range changes {
		b.WriteString("  " + c.String() + "\n")
	}
	return b.String()
}

// canonicalBytes serializes a trace for SNAPSHOT EQUALITY with the Discards
// manifest, the code-identity Stamp, AND the capture-Provenance grade zeroed, so
// equality rests on flow STRUCTURE alone — never on the review-only record of what
// was dropped, the run-varying deploy stamp, nor the capture grade. Provenance is
// deliberately excluded here even though it IS written into the committed golden
// (stampless keeps it): the grade is the impeach corpus's trust input, not a
// behavioral dimension, so two captures of identical behavior at different grades
// (a harness "integration" re-drive vs a "production" deploy) must still assert
// equal — the snapshot gate is about behavior, the impeach corpusDigest is where
// the grade is part of identity. A marshal failure must propagate: swallowed, both
// sides would serialize to "" and the gate would vacuously pass.
//
// Do NOT "fix" this by folding the grade back into equality: that would fail a
// legitimate same-behavior re-drive at a different grade, defeating behavior-purity.
// The grade is still a load-bearing impeach-gating input, but it cannot drift
// silently — because stampless WRITES it, a -update that changes the grade changes
// the committed golden's bytes, so it shows up in the file's git diff and is
// CODEOWNERS-routed for review exactly like any other golden rebase. The mechanical
// guard on the grade is that committed-golden review, not snapshot equality.
func canonicalBytes(t *ir.CanonicalTrace) ([]byte, error) {
	cp := stampless(t)
	cp.Discards = ir.DiscardManifest{}
	cp.Provenance = "" // excluded from equality (behavior-purity); still written to the golden
	return cp.Marshal()
}

// stampless returns a shallow copy of t with the code-identity Stamp zeroed — the
// reducer the GOLDEN WRITER uses. The stamp is run-varying provenance excluded from
// the written golden (mirroring how it is excluded from equality); the capture
// Provenance grade is deliberately NOT zeroed here, so the committed golden CARRIES
// its grade (the impeach corpus reads it) even though canonicalBytes excludes it
// from equality. So a committed golden is byte-identical whether or not the capture
// was stamped, while still self-describing its capture grade.
func stampless(t *ir.CanonicalTrace) ir.CanonicalTrace {
	cp := *t
	cp.Stamp = ""
	return cp
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

// diffLines produces a compact line-oriented view of the first divergence
// between two canonical JSON documents. It is only the fallback when the
// structural diff is empty yet the bytes differ (an edit the differ does not
// model); the prioritized structural change set is the primary output.
func diffLines(want, got string) string {
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
