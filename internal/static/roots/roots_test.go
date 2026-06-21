package roots_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/loader"
	"github.com/jyang234/golang-code-graph/internal/static/roots"
	"github.com/jyang234/golang-code-graph/internal/static/ssabuild"
	"github.com/jyang234/golang-code-graph/internal/static/statictest"
)

func discoverFixture(t *testing.T) *roots.Result {
	t.Helper()
	prog, err := statictest.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return roots.Discover(prog, statictest.Registrars())
}

func TestDiscoverFixtureRoots(t *testing.T) {
	res := discoverFixture(t)

	type rk struct{ kind, name, fqn string }
	got := make(map[rk]bool)
	var primary []rk
	initRoots := 0
	for _, r := range res.Roots {
		key := rk{string(r.Kind), r.Name, r.FQN()}
		got[key] = true
		if r.Kind == roots.KindInit {
			// Every first-party package contributes its synthesized init root,
			// always with an empty Name; the count is the package count, asserted
			// loosely (>0) so the test does not churn when a package is added.
			initRoots++
			if r.Name != "" {
				t.Errorf("init root has a non-empty Name: %+v", r)
			}
			continue
		}
		primary = append(primary, key)
	}

	// The PRIMARY (non-init) root set is pinned exactly: mains, HTTP handlers, and
	// bus consumers. Init roots are partitioned out above.
	want := []rk{
		{"main", "", "example.com/loansvc.main"},
		{"http", "POST /loan-application", "(*example.com/loansvc/internal/handler.App).Create"},
		{"http", "GET /loan-application/{id}/status", "(*example.com/loansvc/internal/handler.App).Status"},
		{"consumer", "payment.settled", "(*example.com/loansvc/internal/consumer.Payments).OnSettled"},
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing root %+v", w)
		}
	}
	if len(primary) != len(want) {
		t.Errorf("got %d primary roots, want %d: %+v", len(primary), len(want), primary)
	}
	// init() is always rooted (it runs unconditionally before main); the fixture
	// spans several first-party packages, so there must be at least one.
	if initRoots == 0 {
		t.Errorf("expected init roots (init runs before main); got none in %+v", res.Roots)
	}
	// The fixture's registrations are all statically resolvable.
	if len(res.BlindSpots) != 0 {
		t.Errorf("unexpected blind spots: %+v", res.BlindSpots)
	}
}

func TestDiscoverResolvesMethodValueToRealMethod(t *testing.T) {
	res := discoverFixture(t)
	for _, r := range res.Roots {
		if r.Kind == roots.KindHTTP && (r.FQN() == "" || containsBound(r.FQN())) {
			t.Errorf("handler root not resolved to real method: %q", r.FQN())
		}
	}
}

func containsBound(s string) bool {
	for i := 0; i+6 <= len(s); i++ {
		if s[i:i+6] == "$bound" {
			return true
		}
	}
	return false
}

func TestDiscoverDeterministic(t *testing.T) {
	prog, err := statictest.Build()
	if err != nil {
		t.Fatal(err)
	}
	first := roots.Discover(prog, statictest.Registrars())
	for i := 0; i < 5; i++ {
		again := roots.Discover(prog, statictest.Registrars())
		if len(again.Roots) != len(first.Roots) {
			t.Fatalf("root count drifted: %d vs %d", len(again.Roots), len(first.Roots))
		}
		for j := range first.Roots {
			if first.Roots[j].FQN() != again.Roots[j].FQN() || first.Roots[j].Kind != again.Roots[j].Kind {
				t.Fatalf("root order drifted at %d: %+v vs %+v", j, first.Roots[j], again.Roots[j])
			}
		}
	}
}

// TestDiscoverBlindSpots synthesizes a module whose handler arguments cannot be
// resolved to concrete functions and checks they are disclosed, not dropped.
func TestDiscoverBlindSpots(t *testing.T) {
	t.Setenv("GOWORK", "off") // analyze the temp module on its own, not via the repo workspace
	dir := t.TempDir()
	write(t, dir, "go.mod", "module dyn\n\ngo 1.24\n")
	write(t, dir, "reg/reg.go", `package reg
type Handler func()
func Register(route string, h Handler) {}
`)
	write(t, dir, "main.go", `package main
import "dyn/reg"
var registry = map[string]reg.Handler{}
func choose() reg.Handler { return func() {} }
func main() {
	reg.Register("GET /a", registry["a"]) // map lookup: unresolvable
	reg.Register("GET /b", choose())      // call result: unresolvable
}
`)

	svc, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	prog, err := ssabuild.Build(svc)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	res := roots.Discover(prog, []roots.Registrar{
		{PkgPath: "dyn/reg", Name: "Register", Kind: roots.KindHTTP, NameArg: 0, HandlerArg: 1},
	})

	if len(res.BlindSpots) != 2 {
		t.Fatalf("got %d blind spots, want 2: %+v", len(res.BlindSpots), res.BlindSpots)
	}
	for _, bs := range res.BlindSpots {
		if bs.Registrar != "dyn/reg.Register" {
			t.Errorf("blind spot registrar = %q", bs.Registrar)
		}
		if bs.Pos == "" {
			t.Errorf("blind spot missing source position: %+v", bs)
		}
	}
	// main is still a root.
	var hasMain bool
	for _, r := range res.Roots {
		if r.Kind == roots.KindMain {
			hasMain = true
		}
	}
	if !hasMain {
		t.Error("main root missing")
	}
}

// TestDiscoverSharedHandlerKeepsEveryRoute synthesizes a module where two routes
// register the same handler function: both must survive as roots. Dedupe keyed
// on the function alone dropped one route from the gated contract, with the
// survivor picked by map iteration order.
func TestDiscoverSharedHandlerKeepsEveryRoute(t *testing.T) {
	res := discoverShared(t)

	got := make(map[string]string, len(res.Roots))
	for _, r := range res.Roots {
		if r.Kind == roots.KindHTTP {
			got[r.Name] = r.FQN()
		}
	}
	for _, route := range []string{"GET /health", "GET /ready"} {
		if got[route] != "dyn.ok" {
			t.Errorf("route %q: got handler %q, want dyn.ok", route, got[route])
		}
	}
	// The shared handler must still be a single graph root.
	fns := res.Funcs()
	seen := make(map[string]bool, len(fns))
	for _, fn := range fns {
		if seen[fn.String()] {
			t.Errorf("Funcs returned duplicate root function %q", fn)
		}
		seen[fn.String()] = true
	}
}

// TestDiscoverSharedHandlerDeterministic re-discovers the shared-handler module:
// the registrations live in two different functions, so a survivor chosen during
// AllFunctions map iteration would differ between runs.
func TestDiscoverSharedHandlerDeterministic(t *testing.T) {
	first := discoverShared(t)
	for i := 0; i < 5; i++ {
		again := discoverShared(t)
		if len(again.Roots) != len(first.Roots) {
			t.Fatalf("root count drifted: %d vs %d", len(again.Roots), len(first.Roots))
		}
		for j := range first.Roots {
			if first.Roots[j].Name != again.Roots[j].Name || first.Roots[j].FQN() != again.Roots[j].FQN() {
				t.Fatalf("root %d drifted: %+v vs %+v", j, first.Roots[j], again.Roots[j])
			}
		}
	}
}

// discoverShared builds a module whose two HTTP routes share one handler,
// registered from two separate functions.
func discoverShared(t *testing.T) *roots.Result {
	t.Helper()
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	write(t, dir, "go.mod", "module dyn\n\ngo 1.24\n")
	write(t, dir, "reg/reg.go", `package reg
type Handler func()
func Register(route string, h Handler) {}
`)
	write(t, dir, "main.go", `package main
import "dyn/reg"
func ok() {}
func registerHealth() { reg.Register("GET /health", ok) }
func registerReady()  { reg.Register("GET /ready", ok) }
func main() {
	registerHealth()
	registerReady()
}
`)

	svc, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	prog, err := ssabuild.Build(svc)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return roots.Discover(prog, []roots.Registrar{
		{PkgPath: "dyn/reg", Name: "Register", Kind: roots.KindHTTP, NameArg: 0, HandlerArg: 1},
	})
}

// buildDeclaredFixture synthesizes the manager-holds-handler idiom: a library
// (inbox) that stores a handler and never dispatches it from in-scope code, a
// consume handler (inbound.Handler.Handle) whose effect cone is therefore
// orphaned, a manager that registers the handler as a struct-field LOAD (m.handler)
// — the value-flow wall the registrar resolver cannot follow — and a background
// worker (subscriptions.Manager.loop) launched with `go`. inbound.Other.Handle is
// a second method of the same name in the same package, for the root-all ambiguity
// case.
func buildDeclaredFixture(t *testing.T) *ssabuild.Program {
	t.Helper()
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	write(t, dir, "go.mod", "module dyn\n\ngo 1.24\n")
	write(t, dir, "inbox/inbox.go", `package inbox
type Handler func(msg string) error
type Inbox struct{ h Handler }
// Register stores the handler; the actual dispatch is out-of-module, so the
// handler is never CALLED through a resolvable in-scope edge.
func Register(ib *Inbox, h Handler) { ib.h = h }
`)
	write(t, dir, "inbound/inbound.go", `package inbound
func Effect() {} // the recovered effect cone
type Handler struct{}
func (h *Handler) Handle(msg string) error { Effect(); return nil }
type Other struct{}
func (o *Other) Handle(msg string) error { return nil }
`)
	write(t, dir, "subscriptions/manager.go", `package subscriptions
import "dyn/inbox"
type Config struct{ Handler inbox.Handler }
type Manager struct {
	handler inbox.Handler
	ib      *inbox.Inbox
}
func New(cfg Config) *Manager { return &Manager{handler: cfg.Handler, ib: &inbox.Inbox{}} }
// Start registers m.handler — a struct-field load the resolver cannot trace back
// to a concrete function — and launches the background worker.
func (m *Manager) Start() {
	inbox.Register(m.ib, m.handler)
	go m.loop()
}
func Reconcile() {}
func (m *Manager) loop() { Reconcile() }
`)
	write(t, dir, "main.go", `package main
import (
	"dyn/inbound"
	"dyn/subscriptions"
)
func main() {
	h := &inbound.Handler{}
	m := subscriptions.New(subscriptions.Config{Handler: h.Handle})
	m.Start()
}
`)
	svc, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	prog, err := ssabuild.Build(svc)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return prog
}

// inboxRegistrar is the inbox.Register hint (a consumer registrar: Register(ib, h)
// — handler at logical arg 1, no route arg).
func inboxRegistrar() []roots.Registrar {
	return []roots.Registrar{
		{PkgPath: "dyn/inbox", Name: "Register", Kind: roots.KindConsumer, NameArg: -1, HandlerArg: 1},
	}
}

func rootByFQN(res *roots.Result, fqn string) *roots.Root {
	for i := range res.Roots {
		if res.Roots[i].FQN() == fqn {
			return &res.Roots[i]
		}
	}
	return nil
}

// TestDiscoverDeclaredEntrypoints is the load-bearing case: a library-dispatched
// callback the registrar resolver provably cannot reach is ORPHANED without a
// declaration (and disclosed as a blind spot), and rooted directly — as a declared
// callback — once declared. A background worker is rooted as a declared worker.
func TestDiscoverDeclaredEntrypoints(t *testing.T) {
	prog := buildDeclaredFixture(t)
	const (
		callbackFQN = "(*dyn/inbound.Handler).Handle"
		workerFQN   = "(*dyn/subscriptions.Manager).loop"
	)

	// Without a declaration: the manager-holds-handler idiom defeats the resolver
	// (the handler reaches Register as a field load), so the callback is orphaned and
	// the registration is disclosed as a blind spot rather than rooted.
	base := roots.Discover(prog, inboxRegistrar())
	if rootByFQN(base, callbackFQN) != nil {
		t.Error("callback was rooted without a declaration; the value-flow wall is supposed to orphan it")
	}
	if len(base.BlindSpots) == 0 {
		t.Error("expected the unresolved manager-held registration to be disclosed as a blind spot")
	}

	// With declarations: callback and worker are rooted directly, by FQN, carrying
	// declared provenance (KindCallback / KindWorker) and the config ref as Name.
	declared := []roots.DeclaredEntrypoint{
		{PkgPath: "dyn/inbound", Symbol: "Handle", Kind: roots.KindCallback, Ref: "dyn/inbound#Handle"},
		{PkgPath: "dyn/subscriptions", Symbol: "loop", Kind: roots.KindWorker, Ref: "dyn/subscriptions#loop"},
	}
	res := roots.Discover(prog, inboxRegistrar(), declared...)

	cb := rootByFQN(res, callbackFQN)
	if cb == nil {
		t.Fatalf("declared callback %s was not rooted: %+v", callbackFQN, res.Roots)
	}
	if cb.Kind != roots.KindCallback || cb.Name != "dyn/inbound#Handle" {
		t.Errorf("callback root = {Kind:%q Name:%q}, want {callback dyn/inbound#Handle}", cb.Kind, cb.Name)
	}
	w := rootByFQN(res, workerFQN)
	if w == nil {
		t.Fatalf("declared worker %s was not rooted: %+v", workerFQN, res.Roots)
	}
	if w.Kind != roots.KindWorker || w.Name != "dyn/subscriptions#loop" {
		t.Errorf("worker root = {Kind:%q Name:%q}, want {worker dyn/subscriptions#loop}", w.Kind, w.Name)
	}
}

// TestDiscoverDeclaredEntrypointRootsAllMatches pins the over-approximate,
// SOUND ambiguity policy: a symbol naming several methods (same name, different
// receivers) roots every match. Rooting only turns provenAbsent → reachable, so an
// extra root can never manufacture a false proof of absence.
func TestDiscoverDeclaredEntrypointRootsAllMatches(t *testing.T) {
	prog := buildDeclaredFixture(t)
	res := roots.Discover(prog, nil, roots.DeclaredEntrypoint{
		PkgPath: "dyn/inbound", Symbol: "Handle", Kind: roots.KindCallback, Ref: "dyn/inbound#Handle",
	})
	for _, want := range []string{"(*dyn/inbound.Handler).Handle", "(*dyn/inbound.Other).Handle"} {
		r := rootByFQN(res, want)
		if r == nil || r.Kind != roots.KindCallback {
			t.Errorf("ambiguous declaration did not root all matches: missing callback root %q in %+v", want, res.Roots)
		}
	}
}

// TestDiscoverDeclaredEntrypointsDeterministic guards the new resolution path:
// declared roots are matched by iterating ssautil.AllFunctions (a map, so a
// non-deterministic order), and the ambiguous symbol "Handle" matches two methods.
// The output must be byte-stable run to run — sortResult resolves on the intrinsic
// (Kind, Name, FQN) key, never iteration order.
func TestDiscoverDeclaredEntrypointsDeterministic(t *testing.T) {
	prog := buildDeclaredFixture(t)
	declared := []roots.DeclaredEntrypoint{
		{PkgPath: "dyn/inbound", Symbol: "Handle", Kind: roots.KindCallback, Ref: "dyn/inbound#Handle"},
		{PkgPath: "dyn/subscriptions", Symbol: "loop", Kind: roots.KindWorker, Ref: "dyn/subscriptions#loop"},
	}
	first := roots.Discover(prog, inboxRegistrar(), declared...)
	for i := 0; i < 5; i++ {
		again := roots.Discover(prog, inboxRegistrar(), declared...)
		if len(again.Roots) != len(first.Roots) {
			t.Fatalf("declared-root count drifted: %d vs %d", len(again.Roots), len(first.Roots))
		}
		for j := range first.Roots {
			if first.Roots[j].FQN() != again.Roots[j].FQN() || first.Roots[j].Kind != again.Roots[j].Kind || first.Roots[j].Name != again.Roots[j].Name {
				t.Fatalf("declared root order drifted at %d: %+v vs %+v", j, first.Roots[j], again.Roots[j])
			}
		}
	}
}

// TestDiscoverDeclaredEntrypointStaleDisclosed pins the fail-closed-and-disclose
// policy for drift: a declaration resolving to no first-party function is surfaced
// as a blind spot, never silently dropped (the silent gap this feature fights).
func TestDiscoverDeclaredEntrypointStaleDisclosed(t *testing.T) {
	prog := buildDeclaredFixture(t)
	res := roots.Discover(prog, nil, roots.DeclaredEntrypoint{
		PkgPath: "dyn/inbound", Symbol: "Nonexistent", Kind: roots.KindCallback, Ref: "dyn/inbound#Nonexistent",
	})
	found := false
	for _, bs := range res.BlindSpots {
		if bs.Registrar == "dyn/inbound#Nonexistent" {
			found = true
		}
	}
	if !found {
		t.Errorf("stale declaration not disclosed as a blind spot: %+v", res.BlindSpots)
	}
}

// TestDiscoverGenericRegistrarMatches pins Ask 3: a generic registrar call
// (Register[T]) is normalized to its generic origin before keying, so it matches a
// registrar keyed on (pkg, "Register"). Before the Origin() normalization the
// instantiation reported Name()=="Register[int]" with an empty package and the
// route silently vanished.
func TestDiscoverGenericRegistrarMatches(t *testing.T) {
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	write(t, dir, "go.mod", "module dyn\n\ngo 1.24\n")
	write(t, dir, "reg/reg.go", `package reg
func Register[T any](route string, h func(T)) {}
`)
	write(t, dir, "main.go", `package main
import "dyn/reg"
func handle(int) {}
func main() { reg.Register[int]("GET /a", handle) }
`)
	svc, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	prog, err := ssabuild.Build(svc)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	res := roots.Discover(prog, []roots.Registrar{
		{PkgPath: "dyn/reg", Name: "Register", Kind: roots.KindHTTP, NameArg: 0, HandlerArg: 1},
	})
	r := rootByFQN(res, "dyn.handle")
	if r == nil || r.Kind != roots.KindHTTP || r.Name != "GET /a" {
		t.Errorf("generic registrar route not discovered via Origin normalization; got %+v", res.Roots)
	}
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
