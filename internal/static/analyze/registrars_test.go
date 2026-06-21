package analyze_test

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/roots"
)

// TestRegistrarsIncludeBuiltinHTTP pins that the built-in HTTP discovery surface
// is always present — stdlib's net/http.HandleFunc and go-chi's per-method router
// functions — independent of config. These determine the entrypoint set every
// reachability verdict starts from, so a silent drop here would shrink the graph.
func TestRegistrarsIncludeBuiltinHTTP(t *testing.T) {
	// A nil config must still yield the built-in registrars (the early-return path).
	if regs := analyze.Registrars(nil); len(regs) == 0 {
		t.Fatal("nil config produced no registrars; built-in HTTP set missing")
	}

	regs := analyze.Registrars(nil)
	if !hasRegistrar(regs, "net/http", "HandleFunc") {
		t.Errorf("built-in net/http.HandleFunc registrar missing: %+v", regs)
	}
	if !hasRegistrar(regs, "github.com/go-chi/chi/v5", "Get") {
		t.Errorf("built-in go-chi Get registrar missing: %+v", regs)
	}
}

// TestRegistrarsFromRouterHints exercises the config-router branch that the
// existing suite did not: each declared method becomes an HTTP registrar whose
// Method is the function name uppercased, and the custom routeArg/handlerArg
// positions are carried through.
func TestRegistrarsFromRouterHints(t *testing.T) {
	cfg := mustConfig(t, ""+
		"static:\n"+
		"  routers:\n"+
		"    - package: example.com/app/router\n"+
		"      methods: [get, Post]\n"+
		"      routeArg: 2\n"+
		"      handlerArg: 3\n")

	regs := analyze.Registrars(cfg)

	get := findRegistrar(regs, "example.com/app/router", "get")
	if get == nil {
		t.Fatalf("router hint did not produce a 'get' registrar: %+v", regs)
	}
	if get.Kind != roots.KindHTTP {
		t.Errorf("router registrar Kind = %q, want %q", get.Kind, roots.KindHTTP)
	}
	if get.Method != "GET" {
		t.Errorf("method not uppercased from function name: Method = %q, want GET", get.Method)
	}
	if get.NameArg != 2 || get.HandlerArg != 3 {
		t.Errorf("custom arg positions not carried: NameArg=%d HandlerArg=%d, want 2/3", get.NameArg, get.HandlerArg)
	}
	if post := findRegistrar(regs, "example.com/app/router", "Post"); post == nil || post.Method != "POST" {
		t.Errorf("second method not registered or not uppercased: %+v", post)
	}
}

// TestRegistrarsDefaultRouterArgs pins the default logical arg positions (route 0,
// handler 1) when the hint omits routeArg/handlerArg.
func TestRegistrarsDefaultRouterArgs(t *testing.T) {
	cfg := mustConfig(t, ""+
		"static:\n"+
		"  routers:\n"+
		"    - package: example.com/app/router\n"+
		"      methods: [Get]\n")
	r := findRegistrar(analyze.Registrars(cfg), "example.com/app/router", "Get")
	if r == nil {
		t.Fatalf("router hint produced no registrar")
	}
	if r.NameArg != 0 || r.HandlerArg != 1 {
		t.Errorf("default arg positions = %d/%d, want 0/1", r.NameArg, r.HandlerArg)
	}
}

// TestRegistrarsBusConsumeWithoutSymbolSkipped pins splitHint's guard: a
// busConsume entry that names a package but no '#symbol' is not a registrar (there
// is no specific subscribe function to match) and must be dropped, not turned into
// a registrar with an empty name that would match nothing or everything.
func TestRegistrarsBusConsumeWithoutSymbolSkipped(t *testing.T) {
	cfg := mustConfig(t, "classify:\n  busConsume: [\"x/bus\"]\n")
	for _, r := range analyze.Registrars(cfg) {
		if r.PkgPath == "x/bus" {
			t.Errorf("busConsume hint without a #symbol produced a registrar: %+v", r)
		}
		if r.Kind == roots.KindConsumer && r.Name == "" {
			t.Errorf("a consumer registrar with an empty name leaked through: %+v", r)
		}
	}
}

// TestDeclaredEntrypointsFromConfig pins the config → declared-root mapping:
// callbacks become KindCallback declarations and workers become KindWorker, each
// split into (pkg, symbol) with the verbatim reference carried as Ref (the disclosed
// Name). This is the wiring that lets a service declare a root by FQN.
func TestDeclaredEntrypointsFromConfig(t *testing.T) {
	cfg := mustConfig(t, ""+
		"entrypoints:\n"+
		"  callbacks:\n"+
		"    - ex.com/svc/internal/inbound#Handle\n"+
		"  workers:\n"+
		"    - ex.com/svc/internal/reconciler#Start\n")

	got := analyze.DeclaredEntrypoints(cfg)
	if len(got) != 2 {
		t.Fatalf("got %d declared entrypoints, want 2: %+v", len(got), got)
	}
	want := map[string]roots.DeclaredEntrypoint{
		"ex.com/svc/internal/inbound#Handle":   {PkgPath: "ex.com/svc/internal/inbound", Symbol: "Handle", Kind: roots.KindCallback, Ref: "ex.com/svc/internal/inbound#Handle"},
		"ex.com/svc/internal/reconciler#Start": {PkgPath: "ex.com/svc/internal/reconciler", Symbol: "Start", Kind: roots.KindWorker, Ref: "ex.com/svc/internal/reconciler#Start"},
	}
	for _, d := range got {
		w, ok := want[d.Ref]
		if !ok {
			t.Errorf("unexpected declared entrypoint: %+v", d)
			continue
		}
		if d != w {
			t.Errorf("declared entrypoint = %+v, want %+v", d, w)
		}
	}

	// A nil config yields no declarations (the early-return path).
	if d := analyze.DeclaredEntrypoints(nil); d != nil {
		t.Errorf("nil config produced declarations: %+v", d)
	}
}

func hasRegistrar(regs []roots.Registrar, pkg, name string) bool {
	return findRegistrar(regs, pkg, name) != nil
}

func findRegistrar(regs []roots.Registrar, pkg, name string) *roots.Registrar {
	for i := range regs {
		if regs[i].PkgPath == pkg && regs[i].Name == name {
			return &regs[i]
		}
	}
	return nil
}
