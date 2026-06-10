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
	d := Compare(base, branch)

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
	// Dependency movement is reported but never breaking.
	if c := find(d, "~", "dependency", "peer1"); c == nil || c.Breaking {
		t.Errorf("changed dependency should be ~ and non-breaking: %v", c)
	}
	if c := find(d, "+", "dependency", "peer2"); c == nil || c.Breaking {
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
	d := Compare(c, c)
	if !d.Empty() || d.Breaking() {
		t.Errorf("identical contracts must produce an empty, non-breaking diff; got %v", d.Changes)
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
	d := Compare(base, branch)
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
