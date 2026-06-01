// Package analyze runs the front half of the static pipeline as one step: read
// the service config, load and SSA-build the unit, discover its roots (using the
// config's bus-consumer hints), and build the call graph. The gated boundary
// contract, the non-gated graph view, and coverage all start from this Result, so
// they agree on the same program, roots, and graph.
package analyze

import (
	"strings"

	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/static/callgraph"
	"github.com/jyang234/golang-code-graph/internal/static/loader"
	"github.com/jyang234/golang-code-graph/internal/static/roots"
	"github.com/jyang234/golang-code-graph/internal/static/ssabuild"
)

// Result is the analyzed service unit.
type Result struct {
	Dir     string
	Config  *config.Config
	Service *loader.Service
	Program *ssabuild.Program
	Roots   *roots.Result
	Graph   *callgraph.Graph
}

// Analyze runs load → SSA → roots → call graph for the service at dir. A missing
// .flowmap.yaml is fine (defaults apply); a malformed one is an error.
func Analyze(dir string) (*Result, error) {
	cfg, err := config.LoadDir(dir)
	if err != nil {
		return nil, err
	}
	svc, err := loader.Load(dir)
	if err != nil {
		return nil, err
	}
	prog, err := ssabuild.Build(svc)
	if err != nil {
		return nil, err
	}
	rs := roots.Discover(prog, Registrars(cfg))
	g, err := callgraph.Build(prog, rs, callgraph.Options{})
	if err != nil {
		return nil, err
	}
	return &Result{Dir: dir, Config: cfg, Service: svc, Program: prog, Roots: rs, Graph: g}, nil
}

// ServiceName is the config's service name, or the module path's last segment.
func (r *Result) ServiceName() string {
	if r.Config != nil && r.Config.Service != "" {
		return r.Config.Service
	}
	mp := r.Program.ModulePath
	if i := strings.LastIndexByte(mp, '/'); i >= 0 {
		return mp[i+1:]
	}
	return mp
}

// Registrars builds the root-discovery hints: built-in HTTP plus the bus
// consumers named in config.classify.busConsume. The convention for a subscribe
// call is Subscribe(topic, handler) — logical args 0 and 1.
func Registrars(cfg *config.Config) []roots.Registrar {
	regs := roots.HTTPRegistrars()
	if cfg == nil {
		return regs
	}
	for _, h := range cfg.Classify.BusConsume {
		pkgPath, name := splitHint(h)
		if name == "" {
			continue // a registrar must name a specific subscribe symbol
		}
		regs = append(regs, roots.Registrar{
			PkgPath: pkgPath, Name: name, Kind: roots.KindConsumer, NameArg: 0, HandlerArg: 1,
		})
	}
	return regs
}

func splitHint(s string) (pkgPath, name string) {
	if i := strings.IndexByte(s, '#'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}
