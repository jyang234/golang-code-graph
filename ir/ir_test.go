package ir

import (
	"bytes"
	"strings"
	"testing"
)

func sampleTrace() *CanonicalTrace {
	return &CanonicalTrace{
		Flow:    "POST /loan-application",
		Service: "loan-svc",
		Root: &CanonicalSpan{
			Op: "HTTP POST /loan-application", Kind: KindServer, Tier: 1, Status: "ok",
			Children: []ChildGroup{
				{Concurrent: true, Members: []*CanonicalSpan{
					{Op: "DB postgres SELECT applicants", Kind: KindClient, Peer: "postgres", Tier: 2,
						Attrs: map[string]string{"db.statement": "SELECT ... WHERE id = ?"}},
					{Op: "HTTP GET credit-bureau /score/{id}", Kind: KindClient, Peer: "credit-bureau", Tier: 1},
				}},
				{Members: []*CanonicalSpan{
					{Op: "PUBLISH loan.approved", Kind: KindProducer, Peer: "Bus", Tier: 1},
				}},
			},
		},
		Discards: DiscardManifest{IDs: "dropped", Timing: "dropped"},
	}
}

func TestMarshalDeterministic(t *testing.T) {
	tr := sampleTrace()
	first, err := tr.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		got, err := tr.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("marshal not deterministic at iteration %d", i)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	tr := sampleTrace()
	b, err := tr.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := back.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, b2) {
		t.Fatalf("round-trip not stable:\n%s\n---\n%s", b, b2)
	}
}

func TestAttrsKeysSorted(t *testing.T) {
	tr := &CanonicalTrace{Flow: "f", Service: "s", Root: &CanonicalSpan{
		Op: "x", Kind: KindClient, Tier: 2,
		Attrs: map[string]string{"z.last": "1", "a.first": "2", "m.mid": "3"},
	}}
	b, err := tr.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	ai, mi, zi := strings.Index(got, "a.first"), strings.Index(got, "m.mid"), strings.Index(got, "z.last")
	if ai < 0 || ai >= mi || mi >= zi {
		t.Fatalf("attrs keys not sorted: %s", got)
	}
}
