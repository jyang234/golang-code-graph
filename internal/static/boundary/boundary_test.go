package boundary_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/boundary"
	"github.com/jyang234/golang-code-graph/internal/static/statictest"
)

func generateFixture(t *testing.T) *boundary.Contract {
	t.Helper()
	c, err := boundary.Generate(statictest.FixtureDir())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return c
}

// TestContractShape checks the fixture's contract reproduces the artifacts §1
// shape: two HTTP entry points, one consumer, three published events, one
// consumed event, two external dependencies — and crucially no DB operations.
func TestContractShape(t *testing.T) {
	c := generateFixture(t)

	if c.Service != "loansvc" {
		t.Errorf("service = %q, want loansvc", c.Service)
	}
	if c.SchemaVersion == "" {
		t.Error("schema_version is empty")
	}

	if got := events(c.Published); !equalStrings(got, []string{"disbursement.initiated", "loan.approved", "loan.declined"}) {
		t.Errorf("published = %v", got)
	}
	if got := events(c.Consumed); !equalStrings(got, []string{"payment.settled"}) {
		t.Errorf("consumed = %v", got)
	}
	if got := events(c.EntryPoints.Consumers); !equalStrings(got, []string{"payment.settled"}) {
		t.Errorf("consumers = %v", got)
	}

	wantHTTP := map[string]string{
		"POST": "/loan-application",
		"GET":  "/loan-application/{id}/status",
	}
	if len(c.EntryPoints.HTTP) != 2 {
		t.Fatalf("http entries = %d, want 2", len(c.EntryPoints.HTTP))
	}
	for _, e := range c.EntryPoints.HTTP {
		if wantHTTP[e.Method] != e.Route || e.Tier != 1 {
			t.Errorf("unexpected http entry %+v", e)
		}
	}

	deps := map[string][]string{}
	for _, d := range c.ExternalDeps {
		if d.Kind != "http" || d.Tier != 1 {
			t.Errorf("unexpected dep %+v", d)
		}
		deps[d.Peer] = d.Ops
	}
	if !equalStrings(deps["credit-bureau"], []string{"GET /score/{id}"}) {
		t.Errorf("credit-bureau ops = %v", deps["credit-bureau"])
	}
	if !equalStrings(deps["payment-gw"], []string{"POST /charge/{id}"}) {
		t.Errorf("payment-gw ops = %v", deps["payment-gw"])
	}
}

// TestDBExcluded asserts the gated contract names no DB operation anywhere — the
// database is owned by the behavioral snapshot, not this artifact.
func TestDBExcluded(t *testing.T) {
	c := generateFixture(t)
	b, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"applicants", "ledger", "audit_log", "SELECT", "INSERT", "UPDATE", "postgres", "database/sql"} {
		if bytes.Contains(b, []byte(banned)) {
			t.Errorf("contract leaks DB detail %q:\n%s", banned, b)
		}
	}
}

// TestNonConstantPublishIsBlindSpot checks the dynamically-named publish lands in
// the gated blind-spot manifest rather than being silently dropped.
func TestNonConstantPublishIsBlindSpot(t *testing.T) {
	c := generateFixture(t)
	var found bool
	for _, bs := range c.BlindSpots {
		if bs.Kind == "NonConstantBoundaryArg" && strings.Contains(bs.Site, "notify") {
			found = true
		}
	}
	if !found {
		t.Errorf("non-constant publish not recorded as a blind spot: %+v", c.BlindSpots)
	}
	// And the dynamic event name must NOT appear as a named published event.
	for _, e := range c.Published {
		if strings.HasPrefix(e.Event, "loan.") && e.Event != "loan.approved" && e.Event != "loan.declined" {
			t.Errorf("unexpected published event from a non-constant publish: %q", e.Event)
		}
	}
}

func TestDeterministicBytes(t *testing.T) {
	a, err := generateFixture(t).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	b, err := generateFixture(t).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("contract bytes differ across regenerations")
	}
}

// TestCommittedContractCurrent is the currency property: the committed artifact in
// the fixture matches a fresh regeneration.
func TestCommittedContractCurrent(t *testing.T) {
	c := generateFixture(t)
	match, err := boundary.Check(statictest.FixtureDir(), c)
	if err != nil {
		t.Fatal(err)
	}
	if !match {
		t.Fatalf("committed %s is stale; regenerate it", boundary.ContractPath(statictest.FixtureDir()))
	}
}

func TestWriteCheckRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := generateFixture(t)
	if err := boundary.Write(dir, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(boundary.ContractPath(dir)); err != nil {
		t.Fatalf("contract not written: %v", err)
	}
	match, err := boundary.Check(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	if !match {
		t.Error("freshly written contract should match itself")
	}
}

// TestBoundaryStableUnderInternalRefactor proves the gate ignores internal
// refactors but catches boundary changes: two services with identical boundaries
// but different internal structure yield identical contracts, while adding a
// published event changes them.
func TestBoundaryStableUnderInternalRefactor(t *testing.T) {
	const cfg = "version: 1\nservice: svc\nclassify:\n  busPublish: [\"svc/bus#Publish\"]\n"
	const busPkg = "package bus\nfunc Publish(event string) {}\n"

	direct := genContract(t, map[string]string{
		"go.mod":        "module svc\n\ngo 1.24\n",
		".flowmap.yaml": cfg,
		"bus/bus.go":    busPkg,
		"main.go":       "package main\nimport \"svc/bus\"\nfunc main() { bus.Publish(\"x\") }\n",
	})
	viaHelper := genContract(t, map[string]string{
		"go.mod":        "module svc\n\ngo 1.24\n",
		".flowmap.yaml": cfg,
		"bus/bus.go":    busPkg,
		"main.go":       "package main\nimport \"svc/bus\"\nfunc emit() { bus.Publish(\"x\") }\nfunc main() { emit() }\n",
	})
	if !bytes.Equal(direct, viaHelper) {
		t.Errorf("internal refactor changed the boundary contract:\n--- direct ---\n%s\n--- via helper ---\n%s", direct, viaHelper)
	}

	extraPublish := genContract(t, map[string]string{
		"go.mod":        "module svc\n\ngo 1.24\n",
		".flowmap.yaml": cfg,
		"bus/bus.go":    busPkg,
		"main.go":       "package main\nimport \"svc/bus\"\nfunc main() { bus.Publish(\"x\"); bus.Publish(\"y\") }\n",
	})
	if bytes.Equal(direct, extraPublish) {
		t.Error("adding a published event did not change the boundary contract")
	}
}

// genContract writes a temp module and returns its marshaled boundary contract.
func genContract(t *testing.T, files map[string]string) []byte {
	t.Helper()
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, err := boundary.Generate(dir)
	if err != nil {
		t.Fatalf("generate %v: %v", files["main.go"], err)
	}
	b, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func events(es []boundary.Event) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Event
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCurrencyGateDetectsStaleCommit proves the inject-drift case for the static
// currency gate: when the committed contract no longer matches a fresh
// regeneration (here, as if a published event was added in code after the
// artifact was last committed), Check reports a mismatch — the signal that fails
// `flowmap boundary --check` in CI.
func TestCurrencyGateDetectsStaleCommit(t *testing.T) {
	dir := t.TempDir()
	current := generateFixture(t)
	if len(current.Published) == 0 {
		t.Skip("fixture has no published events to drop")
	}

	// Commit a stale artifact: the current contract minus one published event.
	stale := *current
	stale.Published = append([]boundary.Event(nil), current.Published[1:]...)
	if err := boundary.Write(dir, &stale); err != nil {
		t.Fatal(err)
	}

	match, err := boundary.Check(dir, current)
	if err != nil {
		t.Fatal(err)
	}
	if match {
		t.Error("a stale committed contract must be detected by the currency gate")
	}

	// And regenerating (writing the current contract) clears it.
	if err := boundary.Write(dir, current); err != nil {
		t.Fatal(err)
	}
	match, err = boundary.Check(dir, current)
	if err != nil {
		t.Fatal(err)
	}
	if !match {
		t.Error("after regeneration the currency gate should pass")
	}
}
