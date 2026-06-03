package boundary_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/boundary"
)

// analyzeModule writes files into a temp module, analyzes it standalone
// (GOWORK=off so the workspace doesn't interfere), and returns its contract.
func analyzeModule(t *testing.T, files map[string]string) *boundary.Contract {
	t.Helper()
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := analyze.Analyze(dir)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	return boundary.Extract(res)
}

func routes(c *boundary.Contract) map[string]string {
	m := map[string]string{}
	for _, e := range c.EntryPoints.HTTP {
		m[e.Route] = e.Method
	}
	return m
}

// TestStdHTTPOapiRouting locks Approach 2: oapi-codegen's std-net/http server
// registers via `m.HandleFunc("GET "+options.BaseURL+"/path", wrapper.X)`. The
// method is in the route string and the route is a 3-operand concatenation with a
// struct-field base URL — recovered from its constant segments.
func TestStdHTTPOapiRouting(t *testing.T) {
	c := analyzeModule(t, map[string]string{
		"go.mod": "module thingsvc\n\ngo 1.24\n",
		"main.go": `package main

import "net/http"

type ServerInterface interface {
	GetThing(w http.ResponseWriter, r *http.Request, id string)
	CreateThing(w http.ResponseWriter, r *http.Request)
}

type wrapper struct{ h ServerInterface }

func (x *wrapper) GetThing(w http.ResponseWriter, r *http.Request)    { x.h.GetThing(w, r, r.PathValue("id")) }
func (x *wrapper) CreateThing(w http.ResponseWriter, r *http.Request) { x.h.CreateThing(w, r) }

type Options struct {
	BaseURL    string
	BaseRouter *http.ServeMux
}

func HandlerWithOptions(si ServerInterface, o Options) http.Handler {
	m := o.BaseRouter
	if m == nil {
		m = http.NewServeMux()
	}
	wr := wrapper{h: si}
	m.HandleFunc("GET "+o.BaseURL+"/thing/{id}", wr.GetThing)
	m.HandleFunc("POST "+o.BaseURL+"/thing", wr.CreateThing)
	return m
}

type impl struct{}

func (impl) GetThing(w http.ResponseWriter, r *http.Request, id string) {}
func (impl) CreateThing(w http.ResponseWriter, r *http.Request)         {}

func main() { HandlerWithOptions(impl{}, Options{}) }
`,
	})
	got := routes(c)
	for route, method := range map[string]string{"/thing/{id}": "GET", "/thing": "POST"} {
		if got[route] != method {
			t.Errorf("route %s: got %q, want %q (all: %+v)", route, got[route], method, c.EntryPoints.HTTP)
		}
	}
}

// TestConfigDeclaredRouter proves Approach 1: a method-named, single-positional-
// handler router (echo's shape) is recognized when declared in static.routers.
func TestConfigDeclaredRouter(t *testing.T) {
	c := analyzeModule(t, map[string]string{
		"go.mod": "module echosvc\n\ngo 1.24\n",
		".flowmap.yaml": `service: echosvc
static:
  routers:
    - package: echosvc
      methods: [GET, POST]
`,
		"main.go": `package main

import "net/http"

// Router mimics echo: a per-method registration function with the handler as a
// single positional argument.
type Router struct{}

func (r *Router) GET(path string, h http.HandlerFunc)  {}
func (r *Router) POST(path string, h http.HandlerFunc) {}

// cacheGet is an unrelated method named like a registrar would be — its second
// arg is not func-typed, so the func-type guard must NOT treat it as a route.
type cache struct{}

func (cache) GET(key string, dest *int) {}

func handleA(w http.ResponseWriter, req *http.Request) {}
func handleB(w http.ResponseWriter, req *http.Request) {}

func main() {
	r := &Router{}
	r.GET("/a/{id}", handleA)
	r.POST("/b", handleB)
	cache{}.GET("k", new(int)) // must not become an entrypoint or a blind spot
}
`,
	})
	got := routes(c)
	for route, method := range map[string]string{"/a/{id}": "GET", "/b": "POST"} {
		if got[route] != method {
			t.Errorf("route %s: got %q, want %q (all: %+v)", route, got[route], method, c.EntryPoints.HTTP)
		}
	}
	if len(c.EntryPoints.HTTP) != 2 {
		t.Errorf("the cache.GET false-match must not add an entrypoint; got %+v", c.EntryPoints.HTTP)
	}
}
