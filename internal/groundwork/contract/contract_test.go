package contract

import "testing"

func find(d Diff, op, surface, name string) *Change {
	for i := range d.Changes {
		c := d.Changes[i]
		if c.Op == op && c.Surface == surface && c.Name == name {
			return &d.Changes[i]
		}
	}
	return nil
}

func TestCompareAddRemoveBreaking(t *testing.T) {
	base := &Contract{
		Service:     "svc",
		EntryPoints: EntryPoints{HTTP: []HTTPEntry{{Method: "GET", Route: "/a"}, {Method: "PUT", Route: "/b"}}},
		Published:   []Event{{Event: "e1"}},
		Consumed:    []Event{{Event: "c1"}},
		ExternalDeps: []ExternalDep{
			{Peer: "peer1", Kind: "http", Ops: []string{"GET /x"}},
		},
	}
	branch := &Contract{
		Service:     "svc",
		EntryPoints: EntryPoints{HTTP: []HTTPEntry{{Method: "GET", Route: "/a"}, {Method: "GET", Route: "/c"}}},
		Published:   []Event{{Event: "e1"}, {Event: "e2"}},
		Consumed:    nil,
		ExternalDeps: []ExternalDep{
			{Peer: "peer1", Kind: "http", Ops: []string{"GET /x", "POST /y"}}, // ops changed
			{Peer: "peer2", Kind: "http", Ops: []string{"GET /z"}},            // added
		},
	}
	d, err := Compare(base, branch)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}

	// Additions are not breaking.
	if c := find(d, "+", "route", "GET /c"); c == nil || c.Breaking {
		t.Errorf("added route should be present and non-breaking: %v", c)
	}
	if c := find(d, "+", "publish", "e2"); c == nil || c.Breaking {
		t.Errorf("added publish should be present and non-breaking: %v", c)
	}
	// Removals of routes/published/consumed are breaking.
	if c := find(d, "-", "route", "PUT /b"); c == nil || !c.Breaking {
		t.Errorf("removed route must be breaking: %v", c)
	}
	if c := find(d, "-", "consume", "c1"); c == nil || !c.Breaking {
		t.Errorf("removed consume must be breaking: %v", c)
	}
	// Dependency movement is reported but never breaking. The change name carries the
	// kind so a peer reached by two kinds renders as two distinct lines.
	if c := find(d, "~", "dependency", "peer1 (http)"); c == nil || c.Breaking {
		t.Errorf("changed dependency should be ~ and non-breaking: %v", c)
	}
	if c := find(d, "+", "dependency", "peer2 (http)"); c == nil || c.Breaking {
		t.Errorf("added dependency should be + and non-breaking: %v", c)
	}
	if !d.Breaking() {
		t.Error("Breaking() should be true (removed route + removed consume)")
	}
}

func TestCompareIdenticalIsEmpty(t *testing.T) {
	c := &Contract{
		Service:     "svc",
		EntryPoints: EntryPoints{HTTP: []HTTPEntry{{Method: "GET", Route: "/a"}}},
		Published:   []Event{{Event: "e1"}},
	}
	d, err := Compare(c, c)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !d.Empty() || d.Breaking() {
		t.Errorf("identical contracts must produce an empty, non-breaking diff; got %v", d.Changes)
	}
}

// M-27: comparing two DIFFERENT services (a copy-paste mix-up of base/branch
// inputs) must refuse, not fabricate a plausible mostly-breaking diff and a
// spurious BLOCK from unrelated surfaces.
func TestCompareRefusesServiceMismatch(t *testing.T) {
	base := &Contract{Service: "paymentsvc", EntryPoints: EntryPoints{HTTP: []HTTPEntry{{Method: "POST", Route: "/charge"}}}}
	branch := &Contract{Service: "ordersvc", EntryPoints: EntryPoints{HTTP: []HTTPEntry{{Method: "GET", Route: "/orders"}}}}
	if _, err := Compare(base, branch); err == nil {
		t.Fatal("Compare accepted two different services; expected a refusal")
	}
	branch.Service = "paymentsvc"
	if _, err := Compare(base, branch); err != nil {
		t.Errorf("Compare refused a same-service pair: %v", err)
	}
}

func TestLoadGolden(t *testing.T) {
	base, err := Load("../../../testdata/groundwork/goldens/layeredsvc.contract.json")
	if err != nil {
		t.Fatalf("load base: %v", err)
	}
	branch, err := Load("../../../testdata/groundwork/goldens/layeredsvc.branch.contract.json")
	if err != nil {
		t.Fatalf("load branch: %v", err)
	}
	d, err := Compare(base, branch)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !d.Breaking() {
		t.Fatalf("the committed branch contract drops the PUT route — diff must be breaking; got %v", d.Changes)
	}
	if find(d, "-", "route", "PUT /users/{id}") == nil {
		t.Errorf("expected the removed PUT route in the diff: %v", d.Changes)
	}
	if find(d, "+", "route", "GET /healthz") == nil {
		t.Errorf("expected the added healthz route in the diff: %v", d.Changes)
	}
}

// A peer reached by two kinds must render as two distinct, kind-labeled dependency
// changes — not collapse to one (the prior peer-only keying dropped the second).
func TestDiffDepsSamePeerTwoKinds(t *testing.T) {
	base := &Contract{Service: "svc"}
	branch := &Contract{Service: "svc", ExternalDeps: []ExternalDep{
		{Peer: "acme", Kind: "http", Ops: []string{"GET /x"}},
		{Peer: "acme", Kind: "blob", Ops: []string{"PutObject"}},
	}}
	d, err := Compare(base, branch)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if c := find(d, "+", "dependency", "acme (http)"); c == nil {
		t.Errorf("missing http dependency change for acme: %+v", d.Changes)
	}
	if c := find(d, "+", "dependency", "acme (blob)"); c == nil {
		t.Errorf("missing blob dependency change for acme (must not collapse with http): %+v", d.Changes)
	}
}
