package sqlfold_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/features"
	"github.com/jyang234/golang-code-graph/internal/static/sqlfold"
)

// dbSite is a recovered DB call site: the enclosing method and the fold's verdict.
type dbSite struct {
	method string
	op     string
	tables []string
	ok     bool
}

// foldFixture analyzes the builder fixture and runs the fold over every
// database/sql query call (ExecContext/QueryContext/QueryRowContext), keyed by the
// enclosing method name so a test can assert one site without depending on order.
func foldFixture(t *testing.T) map[string]dbSite {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", "sqlbuildersvc")
	res, err := analyze.Analyze(dir)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	out := map[string]dbSite{}
	for fn := range ssautil.AllFunctions(res.Program.Prog) {
		if fn.Blocks == nil {
			continue
		}
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				call, ok := instr.(*ssa.Call)
				if !ok {
					continue
				}
				callee := call.Common().StaticCallee()
				if callee == nil || !isDBQuery(callee.Name()) {
					continue
				}
				args := features.StringArgs(call)
				if len(args) == 0 {
					continue
				}
				op, tables, ok := sqlfold.Recover(args[0])
				out[fn.Name()] = dbSite{method: fn.Name(), op: op, tables: tables, ok: ok}
			}
		}
	}
	return out
}

func isDBQuery(name string) bool {
	switch name {
	case "ExecContext", "QueryContext", "QueryRowContext":
		return true
	}
	return false
}

// oneTable asserts the site recovered exactly one named table.
func oneTable(s dbSite) string {
	if len(s.tables) == 1 {
		return s.tables[0]
	}
	return "<not-one:" + sliceStr(s.tables) + ">"
}

func sliceStr(ss []string) string { return strings.Join(ss, ",") }

func TestFoldReadIsConstantSelect(t *testing.T) {
	sites := foldFixture(t)
	s := sites["GetMessage"]
	if !s.ok || s.op != "SELECT" || oneTable(s) != "messages" {
		t.Errorf("GetMessage: want SELECT messages (ok), got %+v", s)
	}
}

func TestFoldRecoversWriteRidingQueryRow(t *testing.T) {
	// INSERT … RETURNING executed via QueryRowContext: the method name says read,
	// only the recovered verb says write. The F-B case.
	s := foldFixture(t)["CreateMessage"]
	if !s.ok || s.op != "INSERT" || oneTable(s) != "messages" {
		t.Errorf("CreateMessage: want INSERT messages (ok), got %+v", s)
	}
}

func TestFoldPromotesWriteWithDynamicTable(t *testing.T) {
	// "DELETE FROM " + table where table is a PARAMETER (unbounded): the verb is
	// constant, the table unresolvable. Write promotion recovers DELETE with no
	// named table.
	s := foldFixture(t)["DeleteByTable"]
	if !s.ok || s.op != "DELETE" || len(s.tables) != 0 {
		t.Errorf("DeleteByTable: want DELETE with no table (ok), got %+v", s)
	}
}

func TestFoldPromotesWriteUnderBranchedTail(t *testing.T) {
	// The verb+table fragment is unconditional; the conditional SET-list tail must
	// not block write promotion, and must not be read as part of the prefix.
	s := foldFixture(t)["UpdatePartial"]
	if !s.ok || s.op != "UPDATE" || oneTable(s) != "accounts" {
		t.Errorf("UpdatePartial: want UPDATE accounts (ok), got %+v", s)
	}
}

// Phase 2: the per-table store's table is a struct field set to one of a finite
// set of string constants. The fold resolves the whole set and names both targets.
func TestFoldResolvesFiniteConstantTableSet(t *testing.T) {
	s := foldFixture(t)["DeleteParticipant"]
	if !s.ok || s.op != "DELETE" {
		t.Fatalf("DeleteParticipant: want DELETE (ok), got %+v", s)
	}
	if got := sliceStr(s.tables); got != "publishers,subscribers" {
		t.Errorf("want resolved table set [publishers subscribers], got %v", s.tables)
	}
}

// Phase 2 soundness: when a struct field is set from a runtime value (the value set
// is not all-constant), the completeness gate must catch the non-constant write and
// abstain on naming — the verb is still promoted (a write), the table left dynamic.
func TestFoldAbstainsOnNonConstantTableField(t *testing.T) {
	s := foldFixture(t)["DeleteDyn"]
	if !s.ok || s.op != "DELETE" {
		t.Fatalf("DeleteDyn: want DELETE (ok), got %+v", s)
	}
	if len(s.tables) != 0 {
		t.Errorf("DeleteDyn: a non-constant table field must not be named, got %v", s.tables)
	}
}

func TestFoldAbstainsWhenVerbIsDynamic(t *testing.T) {
	// The verb itself is a runtime value: nothing is recoverable, so the fold must
	// abstain (fail closed) rather than guess.
	s := foldFixture(t)["ExecOpaque"]
	if s.ok {
		t.Errorf("ExecOpaque: want abstain (ok=false), got %+v", s)
	}
}

// The prime-directive guard: a SELECT-prefixed statement with a dynamic TEXT
// splice must NOT be classified as a read, because the splice could smuggle a
// second, mutating statement. The verb is SELECT but the statement is incomplete,
// so the fold abstains rather than assert a false read (a false non-mutation).
func TestFoldAbstainsOnDynamicTextSpliceInSelect(t *testing.T) {
	s := foldFixture(t)["ReadDynamicFilter"]
	if s.ok {
		t.Errorf("ReadDynamicFilter: a SELECT with a dynamic text splice must abstain, got %+v", s)
	}
}
