package golden

import (
	"os"
	"path/filepath"
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
	if len(md) == 0 || string(md[:15]) != "sequenceDiagram" {
		t.Errorf("flow.md is not a Mermaid diagram: %q", md)
	}
	// A re-compare against the just-written golden passes.
	if err := Compare(tr, dir, tr.Flow, false); err != nil {
		t.Errorf("compare after update should pass: %v", err)
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
