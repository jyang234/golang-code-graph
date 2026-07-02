// Package statictest provides shared fixtures for the static pipeline's tests:
// loading and SSA-building the hermetic loansvc service, and the registrar hints
// that match its router and bus. It avoids importing "testing" so it can be
// reused from any static package's test without turning into a test-only build
// artifact; callers handle the returned errors themselves.
package statictest

import (
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/loader"
	"github.com/jyang234/golang-code-graph/internal/static/roots"
	"github.com/jyang234/golang-code-graph/internal/static/ssabuild"
)

// FixtureDir returns the absolute path to the loansvc fixture module, resolved
// from this file's location so it is independent of the caller's working
// directory.
func FixtureDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", "loansvc")
}

// Load type-checks the fixture service unit.
func Load() (*loader.Service, error) { return loader.Load(FixtureDir()) }

// Build loads and SSA-builds the fixture.
func Build() (*ssabuild.Program, error) {
	svc, err := Load()
	if err != nil {
		return nil, err
	}
	return ssabuild.Build(svc)
}

// Analyze runs the full front half of the static pipeline (config → load → SSA →
// roots → call graph) on the fixture.
func Analyze() (*analyze.Result, error) { return analyze.Analyze(FixtureDir()) }

// FindFunc returns the built function whose fully-qualified name contains substr,
// or nil if none match. Use a substring unique enough to identify one function —
// note that a closure renders as "Parent$1", so a parent's name is a substring of
// its closures; use FindFuncExact when that matters.
//
// It panics if substr matches two functions that are not in a parent/closure
// relationship. Returning the first of a map iteration (the old behavior) made the
// pick — and therefore any test asserting on it — depend on hash-map order; a
// genuinely ambiguous substring is a test bug, so we fail loudly (per CLAUDE.md
// tenet 2) rather than silently pick one run-to-run.
func FindFunc(prog *ssabuild.Program, substr string) *ssa.Function {
	var matches []*ssa.Function
	for fn := range ssautil.AllFunctions(prog.Prog) {
		if strings.Contains(fn.RelString(nil), substr) {
			matches = append(matches, fn)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	// A match that is a closure of another match (its name is "<other>$…") is not a
	// distinct target — the doc contract is that a parent's substring also catches
	// its closures. Reduce to the "top" matches (not nested under another match) and
	// require exactly one.
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].RelString(nil) < matches[j].RelString(nil)
	})
	var top []*ssa.Function
	for _, m := range matches {
		name := m.RelString(nil)
		nested := false
		for _, other := range matches {
			if other == m {
				continue
			}
			if strings.HasPrefix(name, other.RelString(nil)+"$") {
				nested = true
				break
			}
		}
		if !nested {
			top = append(top, m)
		}
	}
	if len(top) > 1 {
		names := make([]string, len(top))
		for i, m := range top {
			names[i] = m.RelString(nil)
		}
		panic("statictest.FindFunc: substr " + strconv.Quote(substr) +
			" is ambiguous, matches: " + strings.Join(names, ", ") +
			" — use a more specific substring or FindFuncExact")
	}
	return top[0]
}

// FindFuncExact returns the built function whose fully-qualified name equals fqn,
// or nil. Unlike FindFunc it never matches a closure of the named function.
func FindFuncExact(prog *ssabuild.Program, fqn string) *ssa.Function {
	for fn := range ssautil.AllFunctions(prog.Prog) {
		if fn.RelString(nil) == fqn {
			return fn
		}
	}
	return nil
}

// Registrars are the registration hints for the fixture: stdlib HTTP plus the
// fixture's own eventbus.Subscribe. In production these bus hints come from
// .flowmap.yaml's classify.busConsume.
func Registrars() []roots.Registrar {
	return append(roots.HTTPRegistrars(), roots.Registrar{
		PkgPath:    "example.com/loansvc/internal/eventbus",
		Name:       "Subscribe",
		Kind:       roots.KindConsumer,
		NameArg:    0,
		HandlerArg: 1,
	})
}
