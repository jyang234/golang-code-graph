// Package loader is the front of the static pipeline: it type-checks one service
// unit with go/packages and hands the result to SSA construction. The unit is a
// single service directory (its "./..." packages); first-party siblings in a
// monorepo resolve through the active go.work / go.mod, while the bus and peer
// services remain the boundary.
//
// It fails loudly. A load error, or any type-check error anywhere in the loaded
// graph, aborts — every downstream stage assumes a complete, well-typed program,
// so a partial load must never be silently analyzed.
package loader

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// loadMode is the fixed set of facts the static pipeline needs from every service
// unit: identity, files, the full import closure, and complete type information
// plus syntax so SSA can be built.
const loadMode = packages.NeedName |
	packages.NeedFiles |
	packages.NeedCompiledGoFiles |
	packages.NeedImports |
	packages.NeedDeps |
	packages.NeedTypes |
	packages.NeedSyntax |
	packages.NeedTypesInfo |
	packages.NeedModule

// Service is one loaded, type-checked service unit.
type Service struct {
	// Dir is the directory the unit was loaded from.
	Dir string
	// Module is the unit's module (nil only if the toolchain reported none).
	Module *packages.Module
	// Packages are the unit's first-party packages, sorted by import path for
	// determinism. Their dependencies are reachable through the import graph but
	// are not listed here.
	Packages []*packages.Package
}

// Load type-checks the packages rooted at dir (the "./..." pattern relative to
// dir) and returns them as one service unit.
func Load(dir string) (*Service, error) {
	cfg := &packages.Config{
		Mode:  loadMode,
		Dir:   dir,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("loader: load %q: %w", dir, err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("loader: no packages found under %q", dir)
	}
	if err := collectErrors(pkgs); err != nil {
		return nil, err
	}

	// Keep only packages with production code. A directory holding solely
	// *_test.go files (e.g. an adopter's flows/ behavioral-gate package) loads as
	// a package with no compiled files; it contributes nothing to the call graph
	// or boundary and must not count toward the analyzed service unit.
	unit := pkgs[:0]
	for _, p := range pkgs {
		if len(p.CompiledGoFiles) > 0 {
			unit = append(unit, p)
		}
	}
	if len(unit) == 0 {
		return nil, fmt.Errorf("loader: no packages with Go source found under %q", dir)
	}

	// packages.Load does not guarantee a stable order; sort so every downstream
	// stage sees the same sequence.
	sort.Slice(unit, func(i, j int) bool { return unit[i].PkgPath < unit[j].PkgPath })

	return &Service{Dir: dir, Module: moduleOf(unit), Packages: unit}, nil
}

// collectErrors walks the whole loaded graph and returns a single error
// summarizing any package or type-check failure. Walking the closure (not just
// the roots) catches a broken dependency that would otherwise corrupt SSA.
func collectErrors(roots []*packages.Package) error {
	var msgs []string
	seen := make(map[string]bool)
	packages.Visit(roots, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			key := p.PkgPath + "|" + e.Error()
			if seen[key] {
				continue
			}
			seen[key] = true
			msgs = append(msgs, fmt.Sprintf("%s: %s", p.PkgPath, e))
		}
	})
	if len(msgs) == 0 {
		return nil
	}
	sort.Strings(msgs)
	const max = 10
	shown := msgs
	suffix := ""
	if len(shown) > max {
		shown = shown[:max]
		suffix = fmt.Sprintf("\n\t(+%d more)", len(msgs)-max)
	}
	return fmt.Errorf("loader: %d type-check/load error(s):\n\t%s%s",
		len(msgs), strings.Join(shown, "\n\t"), suffix)
}

// moduleOf returns the unit's own module: the main module if the toolchain marked
// one, else the module of the shortest-path package (the unit root). Returns nil
// only if no package reported a module.
func moduleOf(pkgs []*packages.Package) *packages.Module {
	var fallback *packages.Module
	var fallbackPkg string
	for _, p := range pkgs {
		if p.Module == nil {
			continue
		}
		if p.Module.Main {
			return p.Module
		}
		// Pick the module of the shortest-path package (the unit root). Compare
		// package path against package path — not the module path, a different
		// namespace — and break ties lexicographically so the result does not
		// depend on the order packages.Load happened to return.
		if fallback == nil || len(p.PkgPath) < len(fallbackPkg) ||
			(len(p.PkgPath) == len(fallbackPkg) && p.PkgPath < fallbackPkg) {
			fallback = p.Module
			fallbackPkg = p.PkgPath
		}
	}
	return fallback
}
