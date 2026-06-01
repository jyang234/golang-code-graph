// Package ssabuild turns a loaded service unit into an SSA program. It builds
// with InstantiateGenerics so calls made through generic code are materialized as
// concrete instantiations and become visible to the call graph.
package ssabuild

import (
	"fmt"
	"sort"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/jyang234/golang-code-graph/internal/static/loader"
)

// Program is the built SSA form of one service unit.
type Program struct {
	// Prog is the whole SSA program: the service packages plus every dependency,
	// fully built.
	Prog *ssa.Program
	// ServicePkgs are the SSA packages for the unit's own first-party packages,
	// sorted by import path. Dependencies live in Prog but are not listed here.
	ServicePkgs []*ssa.Package
	// ModulePath is the unit's module path, used to tell first-party code from
	// dependencies.
	ModulePath string
}

// Build constructs and builds the SSA program for svc.
func Build(svc *loader.Service) (*Program, error) {
	// ssautil.Packages returns one SSA package per initial package, aligned with
	// svc.Packages; an entry is nil for a package that had no syntax (none of the
	// service's own packages do). Dependencies are created in the program but not
	// returned in this slice — exactly the service/dependency split we want.
	prog, ssaPkgs := ssautil.Packages(svc.Packages, ssa.InstantiateGenerics)
	prog.Build()

	service := make([]*ssa.Package, 0, len(ssaPkgs))
	for _, p := range ssaPkgs {
		if p != nil {
			service = append(service, p)
		}
	}
	if len(service) == 0 {
		return nil, fmt.Errorf("ssabuild: no SSA packages built for %q", svc.Dir)
	}
	sort.Slice(service, func(i, j int) bool {
		return service[i].Pkg.Path() < service[j].Pkg.Path()
	})

	modPath := ""
	if svc.Module != nil {
		modPath = svc.Module.Path
	}
	return &Program{Prog: prog, ServicePkgs: service, ModulePath: modPath}, nil
}

// IsFirstParty reports whether pkg belongs to the service unit's module (as
// opposed to stdlib or a third-party dependency). A nil package — synthetic
// functions such as wrappers — is treated as not first-party.
func (p *Program) IsFirstParty(pkg *ssa.Package) bool {
	if pkg == nil || pkg.Pkg == nil || p.ModulePath == "" {
		return false
	}
	path := pkg.Pkg.Path()
	return path == p.ModulePath || hasPathPrefix(path, p.ModulePath)
}

// IsFirstPartyPath reports whether an import path belongs to the service unit's
// module — the string-keyed counterpart of IsFirstParty, for callers that hold a
// package path rather than an *ssa.Package.
func (p *Program) IsFirstPartyPath(path string) bool {
	if path == "" || p.ModulePath == "" {
		return false
	}
	return path == p.ModulePath || hasPathPrefix(path, p.ModulePath)
}

// hasPathPrefix reports whether importPath is the module path followed by a "/"
// segment boundary, so "example.com/loansvc" matches its sub-packages but not an
// unrelated "example.com/loansvc-extra".
func hasPathPrefix(importPath, modulePath string) bool {
	return len(importPath) > len(modulePath) &&
		importPath[:len(modulePath)] == modulePath &&
		importPath[len(modulePath)] == '/'
}
