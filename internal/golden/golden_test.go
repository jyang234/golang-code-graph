package golden

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/ir"
)

func sample() *ir.CanonicalTrace {
	return &ir.CanonicalTrace{
		Flow:    "POST /loan-application",
		Service: "loansvc",
		Root: &ir.CanonicalSpan{
			Op: "HTTP POST /loan-application", Kind: ir.KindServer, Tier: 1, Status: "ok",
			Children: []ir.ChildGroup{
				{Members: []*ir.CanonicalSpan{{Op: "PUBLISH loan.approved", Kind: ir.KindProducer, Peer: "Bus", Tier: 1}}},
			},
		},
		Discards: ir.DiscardManifest{IDs: "dropped", Timing: "dropped"},
	}
}

// TestProvenanceWrittenButNotCompared pins the deliberate asymmetry the
// behavioral-impeachment audit depends on: the capture-fidelity grade is WRITTEN into
// the committed golden (so the corpus self-describes its trust grade, and a -update
// that changes it is git-visible / CODEOWNERS-routed) yet EXCLUDED from snapshot
// EQUALITY (so two captures of identical behavior at different grades still assert
// equal — behavior-purity). A refactor that folds the grade into equality, or drops
// it from the written form, breaks exactly one of the two halves and fails here.
func TestProvenanceWrittenButNotCompared(t *testing.T) {
	prod, integ := sample(), sample()
	prod.Provenance, integ.Provenance = "production", "integration"

	// NOT compared: equality is blind to the grade.
	pb, err := canonicalBytes(prod)
	if err != nil {
		t.Fatal(err)
	}
	ib, err := canonicalBytes(integ)
	if err != nil {
		t.Fatal(err)
	}
	if string(pb) != string(ib) {
		t.Error("canonicalBytes differ on capture grade alone — behavior-purity broken (grade folded into equality)")
	}

	// WRITTEN: the committed golden's bytes carry the grade, so a grade change shows
	// up in the file's git diff (the mechanical guard is committed-golden review).
	pp, ii := stampless(prod), stampless(integ)
	pw, err := pp.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	iw, err := ii.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(pw) == string(iw) {
		t.Error("the written golden does not carry the capture grade — a grade change would be invisible in the committed file's git diff")
	}
}

// TestStampNeitherWrittenNorCompared completes the three-field treatment: the
// run-varying deploy Stamp is excluded from BOTH the written golden and equality
// (identity is injected at audit time, not baked into the committed snapshot).
func TestStampNeitherWrittenNorCompared(t *testing.T) {
	a, b := sample(), sample()
	a.Stamp, b.Stamp = "sha-A", "sha-B"

	ab, err := canonicalBytes(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := canonicalBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ab) != string(bb) {
		t.Error("Stamp leaked into snapshot equality")
	}

	as, bs := stampless(a), stampless(b)
	aw, err := as.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	bw, err := bs.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(aw) != string(bw) {
		t.Error("Stamp leaked into the written golden")
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	tr := sample()
	if err := Compare(tr, dir, tr.Flow, true); err != nil {
		t.Fatalf("update: %v", err)
	}
	// Both artifacts are written.
	stem := filepath.Join(dir, Slug(tr.Flow))
	if _, err := os.Stat(stem + ".golden.json"); err != nil {
		t.Errorf("golden json not written: %v", err)
	}
	md, err := os.ReadFile(stem + ".flow.md")
	if err != nil {
		t.Fatalf("flow.md not written: %v", err)
	}
	if !strings.HasPrefix(string(md), "```mermaid\nsequenceDiagram") {
		t.Errorf("flow.md is not a fenced Mermaid diagram: %q", md)
	}
	// A re-compare against the just-written golden passes.
	if err := Compare(tr, dir, tr.Flow, false); err != nil {
		t.Errorf("compare after update should pass: %v", err)
	}
}

// TestStampExcludedFromEquality is the determinism guard for the code-identity
// stamp: a stamped LIVE capture must assert equal to its STAMPLESS committed
// golden (the stamp is run-varying provenance, like Discards), and the written
// golden must itself be stampless — so a committed golden is byte-identical
// whether or not the capture carried a stamp, and never churns per deploy.
func TestStampExcludedFromEquality(t *testing.T) {
	dir := t.TempDir()
	tr := sample() // stampless
	if err := Compare(tr, dir, tr.Flow, true); err != nil {
		t.Fatal(err)
	}
	// The written golden carries no stamp even if the capture had one.
	stamped := sample()
	stamped.Stamp = "deadbeefcafe"
	dir2 := t.TempDir()
	if err := Compare(stamped, dir2, stamped.Flow, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir2, Slug(stamped.Flow)+".golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "deadbeefcafe") {
		t.Errorf("committed golden leaked the run-varying stamp:\n%s", raw)
	}
	// A stamped capture compares equal to the stampless committed golden.
	if err := Compare(stamped, dir, tr.Flow, false); err != nil {
		t.Errorf("stamp must be excluded from equality, got: %v", err)
	}
}

func TestComparesIgnoringDiscards(t *testing.T) {
	dir := t.TempDir()
	tr := sample()
	if err := Compare(tr, dir, tr.Flow, true); err != nil {
		t.Fatal(err)
	}
	// Same flow, different Discards manifest → still equal.
	other := sample()
	other.Discards = ir.DiscardManifest{IDs: "dropped", Timing: "dropped", Redactions: []string{"customer.email"}, Loops: []string{"DB postgres INSERT items"}}
	if err := Compare(other, dir, tr.Flow, false); err != nil {
		t.Errorf("Discards must be excluded from equality, got: %v", err)
	}
}

func TestDetectsBehaviorChange(t *testing.T) {
	dir := t.TempDir()
	tr := sample()
	if err := Compare(tr, dir, tr.Flow, true); err != nil {
		t.Fatal(err)
	}
	// A structural change (an added publish) must fail the assertion.
	changed := sample()
	changed.Root.Children = append(changed.Root.Children, ir.ChildGroup{
		Members: []*ir.CanonicalSpan{{Op: "PUBLISH disbursement.initiated", Kind: ir.KindProducer, Peer: "Bus", Tier: 1}},
	})
	err := Compare(changed, dir, tr.Flow, false)
	if err == nil {
		t.Fatal("expected a mismatch error for the added publish")
	}
	if !contains(err.Error(), "disbursement.initiated") {
		t.Errorf("diff should mention the added op, got: %v", err)
	}
}

// An edit the structural differ does not model — here a span's owning-service
// label — still changes the canonical bytes, so the assertion must fall back
// to the line diff: the reviewer sees what diverged, never a failed gate with
// an empty change set.
func TestUnmodeledEditFallsBackToLineDiff(t *testing.T) {
	dir := t.TempDir()
	tr := sample()
	if err := Compare(tr, dir, tr.Flow, true); err != nil {
		t.Fatal(err)
	}
	moved := sample()
	moved.Root.Children[0].Members[0].Service = "other-svc"
	err := Compare(moved, dir, tr.Flow, false)
	if err == nil {
		t.Fatal("expected a mismatch error for the owning-service change")
	}
	if !contains(err.Error(), "+ ") || !contains(err.Error(), "other-svc") {
		t.Errorf("line-diff fallback should show the diverging line, got: %v", err)
	}
}

func TestMissingGolden(t *testing.T) {
	dir := t.TempDir()
	tr := sample()
	if err := Compare(tr, dir, tr.Flow, false); err == nil {
		t.Fatal("expected an error when no golden exists")
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"POST /loan-application":            "post_loan_application",
		"GET /loan-application/{id}/status": "get_loan_application_id_status",
		"consume payment.settled":           "consume_payment_settled",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
