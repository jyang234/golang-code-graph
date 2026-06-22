package rebind_test

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/callgraph"
	"github.com/jyang234/golang-code-graph/internal/static/rebind"
)

// cmdSpec is one generated command: which runner shape it dispatches through, and a
// BITMASK of escape modes applied to its closure (0 = confined).
type cmdSpec struct {
	iface bool
	mask  int
}

// The five escape modes the fuzz composes onto a closure. Each adds at least one
// referrer to the closure value other than the rebind call, so confined() must reject
// it. They are composed (a closure can carry several at once), which is the value over a
// fixed fixture: it stresses the guard under COMPOUND escapes and orderings.
const (
	escStoreField  = 1 << iota // c.saved = fn
	escStoreGlobal             // sink = fn
	escChannel                 // c.ch <- fn
	escCapture                 // captured into another closure
	escSecondCall              // passed to a second invoker call
	escAll         = escStoreField | escStoreGlobal | escChannel | escCapture | escSecondCall
)

// FuzzRebindConfinement is the soundness fuzz for the de-union escape guard. For a seeded
// program of commands with random (runner shape × compound escape mask) shapes, it asserts
// the INVARIANT that makes --rebind sound: a closure is de-unioned IFF it is confined
// (mask == 0). The dangerous direction is a de-unioned ESCAPED closure — that removes a
// union edge the closure could still be reached through (a false absence, a must_not_reach
// flip). Because an escaped closure that is never de-unioned keeps its full union, the
// "escaped ⇒ not de-unioned" half is exactly the no-false-absence guarantee; the
// "confined ⇒ de-unioned" half pins that the guard is not vacuously safe.
//
// It runs its seed corpus under `go test` (deterministic) and explores further under
// `-fuzz`. Each input synthesises a small module, builds its SSA, and runs rebind.Compute.
func FuzzRebindConfinement(f *testing.F) {
	for _, s := range []int64{1, 2, 3, 5, 8, 13, 21, 34, 55, 89, 144, 233, 377, 610, 987, 1597} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, seed int64) {
		rng := rand.New(rand.NewSource(seed))
		specs := make([]cmdSpec, 8)
		// Guarantee a confined command of EACH runner shape, so every input exercises the
		// positive (de-union fires) path for both the static and interface runner.
		specs[0] = cmdSpec{iface: false, mask: 0}
		specs[1] = cmdSpec{iface: true, mask: 0}
		for i := 2; i < len(specs); i++ {
			specs[i] = cmdSpec{iface: rng.Intn(2) == 0, mask: rng.Intn(escAll + 1)}
		}

		dir := t.TempDir()
		src := genRebindProgram(specs)
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/genrebind\n\ngo 1.24.7\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}

		res, err := analyze.Analyze(dir, callgraph.Options{Algo: callgraph.AlgoVTA})
		if err != nil {
			t.Fatalf("analyze generated program: %v\n%s", err, src)
		}
		plan := rebind.Compute(res)

		deUnioned := map[int]bool{}
		for _, e := range plan.Add {
			for i := range specs {
				if strings.Contains(e.From, fmt.Sprintf("Cmd%d)", i)) {
					deUnioned[i] = true
				}
			}
		}
		for i, s := range specs {
			confined := s.mask == 0
			switch {
			case confined && !deUnioned[i]:
				t.Errorf("Cmd%d is confined (iface=%v) but was NOT de-unioned\n%s", i, s.iface, src)
			case !confined && deUnioned[i]:
				t.Errorf("Cmd%d escapes (mask=%d, iface=%v) but WAS de-unioned — a false-absence risk\n%s", i, s.mask, s.iface, src)
			}
		}
	})
}

// genRebindProgram emits a self-contained main package: one shared interface runner with a
// single directly-invoking implementation, and one command per spec whose closure carries
// the spec's escape modes and is handed to the static or interface runner.
func genRebindProgram(specs []cmdSpec) string {
	var b strings.Builder
	b.WriteString("package main\n\nimport (\n\t\"context\"\n\t\"database/sql\"\n)\n\n")
	b.WriteString("type Exec struct{ db *sql.DB }\n")
	b.WriteString("type TxRunner interface{ RunInTx(fn func(*Exec) error) error }\n")
	b.WriteString("type SQLRunner struct{ db *sql.DB }\n")
	b.WriteString("func (r *SQLRunner) RunInTx(fn func(*Exec) error) error { e := &Exec{db: r.db}; return fn(e) }\n")
	b.WriteString("type Store struct{ db *sql.DB }\n")
	b.WriteString("var sink func(*Exec) error\n\n")
	for i, s := range specs {
		runnerCall := "c.us.RunInTx(fn)"
		if s.iface {
			runnerCall = "c.u.RunInTx(fn)"
		}
		fmt.Fprintf(&b, "func (st *Store) write%d() error { _, err := st.db.ExecContext(context.Background(), \"INSERT INTO t%d VALUES (1)\"); return err }\n", i, i)
		fmt.Fprintf(&b, "type Cmd%d struct { u TxRunner; us *SQLRunner; st *Store; ch chan func(*Exec) error; saved func(*Exec) error }\n", i)
		fmt.Fprintf(&b, "func (c *Cmd%d) Handle() error {\n\tfn := func(e *Exec) error { return c.st.write%d() }\n", i, i)
		if s.mask&escStoreField != 0 {
			b.WriteString("\tc.saved = fn\n")
		}
		if s.mask&escStoreGlobal != 0 {
			b.WriteString("\tsink = fn\n")
		}
		if s.mask&escChannel != 0 {
			b.WriteString("\tc.ch <- fn\n")
		}
		if s.mask&escCapture != 0 {
			b.WriteString("\twrap := func() error { return fn(nil) }\n\t_ = wrap\n")
		}
		if s.mask&escSecondCall != 0 {
			fmt.Fprintf(&b, "\t_ = %s\n", runnerCall)
		}
		fmt.Fprintf(&b, "\treturn %s\n}\n", runnerCall)
	}
	b.WriteString("func main() {\n\tst := &Store{}\n\tr := &SQLRunner{}\n")
	for i := range specs {
		fmt.Fprintf(&b, "\t_ = (&Cmd%d{u: r, us: r, st: st, ch: make(chan func(*Exec) error, 1)}).Handle()\n", i)
	}
	b.WriteString("}\n")
	return b.String()
}
