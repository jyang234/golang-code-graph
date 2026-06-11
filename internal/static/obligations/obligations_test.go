package obligations

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/jyang234/golang-code-graph/internal/config"
)

// buildSSA compiles one inline source file into SSA and returns every function
// in the package (methods included).
func buildSSA(t *testing.T, src string) []*ssa.Function {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pkg := types.NewPackage("example.com/fix", "")
	spkg, _, err := ssautil.BuildPackage(
		&types.Config{Importer: importer.Default()}, fset, pkg, []*ast.File{f},
		ssa.SanityCheckFunctions|ssa.InstantiateGenerics)
	if err != nil {
		t.Fatalf("build SSA: %v", err)
	}
	var fns []*ssa.Function
	for fn := range ssautil.AllFunctions(spkg.Prog) {
		if fn.Pkg == spkg {
			fns = append(fns, fn)
		}
	}
	return fns
}

const txSrc = `package fix

type Tx struct{ closed bool }
type Store struct{ tx *Tx }

func (s *Store) BeginTx() (*Tx, error) { return &Tx{}, nil }
func (t *Tx) Commit() error            { t.closed = true; return nil }
func (t *Tx) Rollback()                { t.closed = true }

func debit(t *Tx) error  { return nil }
func credit(t *Tx) error { return nil }

// The worked example: the debit-failure return leaks; the acquire's own
// failure branch must NOT count as a leak (a failed acquire holds nothing).
func Transfer(s *Store) error {
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	if err := debit(tx); err != nil {
		return err
	}
	if err := credit(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func TransferDefer(s *Store) error {
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := debit(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Ownership leaves the function: the open tx is returned.
func TransferOwn(s *Store) (*Tx, error) {
	tx, err := s.BeginTx()
	if err != nil {
		return nil, err
	}
	return tx, nil
}

func TransferRecover(s *Store) error {
	defer func() { _ = recover() }()
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	return tx.Commit()
}

func TransferLoop(s *Store, n int) error {
	for i := 0; i < n; i++ {
		tx, err := s.BeginTx()
		if err != nil {
			return err
		}
		if err := debit(tx); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
`

func txRule() config.ObligationRule {
	return config.ObligationRule{
		Name:    "tx-must-close",
		Acquire: "example.com/fix#BeginTx",
		Release: []string{"example.com/fix#Commit", "example.com/fix#Rollback"},
	}
}

// one returns the single finding for the free function named name in the
// fixture package, by exact FQN.
func one(t *testing.T, fs []Finding, name string) Finding {
	t.Helper()
	fqn := "example.com/fix." + name
	var got []Finding
	for _, f := range fs {
		if f.Fn == fqn {
			got = append(got, f)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%s: want 1 finding, got %v (all: %v)", fqn, got, fs)
	}
	return got[0]
}

func TestMustReleaseVerdicts(t *testing.T) {
	fns := buildSSA(t, txSrc)
	fs := Check([]config.ObligationRule{txRule()}, fns, "")

	leak := one(t, fs, "Transfer")
	if leak.Status != Violated {
		t.Errorf("Transfer = %s (%s), want VIOLATED", leak.Status, leak.Detail)
	}
	if !strings.Contains(leak.Detail, "fixture.go:") {
		t.Errorf("Transfer detail = %q, want an exit site", leak.Detail)
	}
	if !strings.HasPrefix(leak.Site, "fixture.go:") {
		t.Errorf("Transfer site = %q, want fixture.go:NN", leak.Site)
	}

	if f := one(t, fs, "TransferDefer"); f.Status != Satisfied {
		t.Errorf("TransferDefer = %s (%s), want SATISFIED (defer covers all exits)", f.Status, f.Detail)
	}
	if f := one(t, fs, "TransferOwn"); f.Status != CantProve || !strings.Contains(f.Detail, "returned") {
		t.Errorf("TransferOwn = %s (%s), want CANT-PROVE/returned", f.Status, f.Detail)
	}
	if f := one(t, fs, "TransferRecover"); f.Status != CantProve || !strings.Contains(f.Detail, "recover") {
		t.Errorf("TransferRecover = %s (%s), want CANT-PROVE/recover", f.Status, f.Detail)
	}
	if f := one(t, fs, "TransferLoop"); f.Status != Satisfied {
		t.Errorf("TransferLoop = %s (%s), want SATISFIED (release on every arm, loop back-edge is not an exit)", f.Status, f.Detail)
	}
}

// Acquiring through an interface must bind the rule both ways: the rule names
// the interface's package#method and the call site is invoke-mode.
func TestMustReleaseThroughInterface(t *testing.T) {
	src := `package fix

type Tx struct{}
func (t *Tx) Commit() error { return nil }

type Beginner interface{ BeginTx() (*Tx, error) }

func Run(b Beginner) error {
	tx, err := b.BeginTx()
	if err != nil {
		return err
	}
	_ = tx
	return nil // leak: no release on this path
}
`
	fns := buildSSA(t, src)
	fs := Check([]config.ObligationRule{{
		Name:    "tx-must-close",
		Acquire: "example.com/fix#BeginTx",
		Release: []string{"example.com/fix#Commit"},
	}}, fns, "")
	if f := one(t, fs, "Run"); f.Status != Violated {
		t.Errorf("invoke-mode acquire = %s (%s), want VIOLATED", f.Status, f.Detail)
	}
}

const auditSrc = `package fix

func audit(event string)   {}
func publish(event string) {}

// SATISFIED: the audit dominates the publish.
func Disburse(ok bool) {
	audit("loan.approved")
	if ok {
		publish("loan.approved")
	}
}

// VIOLATED: one branch publishes without auditing.
func DisburseRacy(ok bool) {
	if ok {
		audit("loan.approved")
	}
	publish("loan.approved")
}

// Same block: order decides.
func SameBlockGood() { audit("x"); publish("x") }
func SameBlockBad()  { publish("x"); audit("x") }

// A deferred audit runs at exit, AFTER the publish: it must not satisfy.
func DeferredAudit() {
	defer audit("x")
	publish("x")
}
`

func auditRule() config.ObligationRule {
	return config.ObligationRule{
		Name:    "audit-before-publish",
		Require: "example.com/fix#audit",
		Before:  "example.com/fix#publish",
	}
}

func TestMustPrecedeVerdicts(t *testing.T) {
	fns := buildSSA(t, auditSrc)
	fs := Check([]config.ObligationRule{auditRule()}, fns, "")

	cases := map[string]Status{
		"Disburse":      Satisfied,
		"DisburseRacy":  Violated,
		"SameBlockGood": Satisfied,
		"SameBlockBad":  Violated,
		"DeferredAudit": Violated,
	}
	for fn, want := range cases {
		if f := one(t, fs, fn); f.Status != want {
			t.Errorf("%s = %s (%s), want %s", fn, f.Status, f.Detail, want)
		}
	}
}

func TestUnmatchedRuleDisclosed(t *testing.T) {
	fns := buildSSA(t, txSrc)
	fs := Check([]config.ObligationRule{{
		Name:    "renamed-away",
		Acquire: "example.com/fix#BeginTransaction", // no such symbol anymore
		Release: []string{"example.com/fix#Commit"},
	}}, fns, "")
	if len(fs) != 1 || fs[0].Status != Unmatched || fs[0].Fn != "" {
		t.Fatalf("want one UNMATCHED finding with no fn, got %v", fs)
	}
	if !strings.Contains(fs[0].Detail, "inert") {
		t.Errorf("detail = %q, want the inert-guardrail wording", fs[0].Detail)
	}
}

func TestCheckDeterministic(t *testing.T) {
	fns := buildSSA(t, txSrc)
	rules := []config.ObligationRule{txRule()}
	a, b := Check(rules, fns, ""), Check(rules, fns, "")
	if len(a) != len(b) {
		t.Fatalf("non-deterministic finding count: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic finding %d: %v vs %v", i, a[i], b[i])
		}
	}
}

// RF-4: the six idioms the review confirmed empirically broken, locked as
// permanent fixtures with their post-fix verdicts.
const rf4Src = `package fix

type Tx struct{ closed bool }
type TxError struct{ msg string }

func (e *TxError) Error() string { return e.msg }

type Store struct{ tx *Tx }

func (s *Store) BeginTx() (*Tx, error)     { return &Tx{}, nil }
func (s *Store) BeginTxC() (*Tx, *TxError) { return &Tx{}, nil }
func (s *Store) Acquire() error            { return nil }
func (s *Store) Release()                  {}
func (t *Tx) Commit() error                { t.closed = true; return nil }
func (t *Tx) Rollback() error              { t.closed = true; return nil }

func annotate(err error) error { return err }
func handle()                  { _ = recover() }

// Single-result error acquire: the failure branch must still be pruned.
func HoldSem(s *Store) error {
	if err := s.Acquire(); err != nil {
		return err
	}
	defer s.Release()
	return nil
}

// Named result captured by an annotating defer: err lives in an alloc and the
// nil-test compares a load — the web must still recognize the failure branch.
func TransferAnnotate(s *Store) (err error) {
	defer func() { err = annotate(err) }()
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return tx.Commit()
}

// Concrete error type: *TxError implements error; it is the err, not the
// resource, and its failure branch prunes.
func TransferConcrete(s *Store) error {
	tx, terr := s.BeginTxC()
	if terr != nil {
		return terr
	}
	defer func() { _ = tx.Rollback() }()
	return tx.Commit()
}

// The errcheck-clean cleanup idiom: a deferred closure releasing the captured
// resource is in-frame and credited, not an "escape".
func TransferClosure(s *Store) error {
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return tx.Commit()
}

// recover in a deferred NAMED function rejoins this frame: must abstain.
func TransferRecoverNamed(s *Store) error {
	defer handle()
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	return tx.Commit()
}
`

func TestRF4ReleaseIdioms(t *testing.T) {
	fns := buildSSA(t, rf4Src)
	rules := []config.ObligationRule{
		{Name: "tx-must-close", Acquire: "example.com/fix#BeginTx",
			Release: []string{"example.com/fix#Commit", "example.com/fix#Rollback"}},
		{Name: "txc-must-close", Acquire: "example.com/fix#BeginTxC",
			Release: []string{"example.com/fix#Commit", "example.com/fix#Rollback"}},
		{Name: "sem-must-release", Acquire: "example.com/fix#Acquire",
			Release: []string{"example.com/fix#Release"}},
	}
	fs := Check(rules, fns, "")

	want := map[string]Status{
		"HoldSem":              Satisfied,
		"TransferAnnotate":     Satisfied,
		"TransferConcrete":     Satisfied,
		"TransferClosure":      Satisfied,
		"TransferRecoverNamed": CantProve,
	}
	for fn, wantStatus := range want {
		if f := one(t, fs, fn); f.Status != wantStatus {
			t.Errorf("%s = %s (%s), want %s", fn, f.Status, f.Detail, wantStatus)
		}
	}
	if f := one(t, fs, "TransferRecoverNamed"); !strings.Contains(f.Detail, "recover") {
		t.Errorf("recover abstention reason = %q", f.Detail)
	}
}

// RF-4: a deferred Before still happens and still needs its Require — and a
// rule whose only B is deferred must not read as UNMATCHED/inert.
func TestRF4DeferredBefore(t *testing.T) {
	src := `package fix

func audit(event string)   {}
func publish(event string) {}

func DeferredPublish()        { defer publish("x") }
func DeferredPublishAudited() { audit("x"); defer publish("x") }
`
	fns := buildSSA(t, src)
	fs := Check([]config.ObligationRule{{
		Name: "audit-before-publish", Require: "example.com/fix#audit", Before: "example.com/fix#publish",
	}}, fns, "")

	if f := one(t, fs, "DeferredPublish"); f.Status != Violated {
		t.Errorf("unaudited deferred publish = %s, want VIOLATED (and not UNMATCHED)", f.Status)
	}
	if f := one(t, fs, "DeferredPublishAudited"); f.Status != Satisfied {
		t.Errorf("audit-dominated deferred publish = %s (%s), want SATISFIED", f.Status, f.Detail)
	}
	for _, f := range fs {
		if f.Status == Unmatched {
			t.Errorf("rule with deferred-only B sites reported inert: %v", f)
		}
	}
}

// RF-5: the site ladder is total — no rung emits a machine-specific path, and
// no input collapses identity onto "".
func TestSiteLadder(t *testing.T) {
	fns := buildSSA(t, txSrc)
	var fn *ssa.Function
	for _, f := range fns {
		if f.Name() == "Transfer" {
			fn = f
			break
		}
	}
	if fn == nil {
		t.Fatal("Transfer not built")
	}

	// Rung 3: an invalid position yields a synthetic-but-unique identity.
	if got := site(fn, token.NoPos, "/anywhere", 2); got != "example.com/fix:synthetic#2" {
		t.Errorf("synthetic site = %q", got)
	}
	// Rung 2: a file outside baseDir gets the portable package-qualified form,
	// never the raw absolute filename.
	got := site(fn, fn.Pos(), "/definitely/not/a/parent", 0)
	if !strings.HasPrefix(got, "example.com/fix/fixture.go:") {
		t.Errorf("out-of-dir site = %q, want pkg-qualified portable form", got)
	}
	if strings.Contains(got, "/definitely") || strings.HasPrefix(got, "/") {
		t.Errorf("out-of-dir site leaked a machine path: %q", got)
	}
}
