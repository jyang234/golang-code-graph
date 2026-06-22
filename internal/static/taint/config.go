package taint

import (
	"fmt"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/config"
)

// FromConfig parses a service's .flowmap.yaml taint section into a Config. A
// function spec is "importpath#Name"; a field spec is "importpath#Type.Field". A
// malformed spec is a load-time error (fail closed — a typo must not silently widen
// or narrow the source/sink set), not a silently dropped entry.
func FromConfig(cfg *config.Config) (Config, error) {
	if cfg == nil {
		return Config{}, nil
	}
	var out Config
	for _, s := range cfg.Taint.SourceFuncs {
		fs, err := parseFuncSpec(s)
		if err != nil {
			return Config{}, fmt.Errorf("taint.sourceFuncs: %w", err)
		}
		out.SourceFuncs = append(out.SourceFuncs, fs)
	}
	for _, s := range cfg.Taint.Sinks {
		fs, err := parseFuncSpec(s)
		if err != nil {
			return Config{}, fmt.Errorf("taint.sinks: %w", err)
		}
		out.Sinks = append(out.Sinks, fs)
	}
	for _, s := range cfg.Taint.SourceFields {
		fs, err := parseFieldSpec(s)
		if err != nil {
			return Config{}, fmt.Errorf("taint.sourceFields: %w", err)
		}
		out.SourceFields = append(out.SourceFields, fs)
	}
	return out, nil
}

// parseFuncSpec parses "importpath#Name".
func parseFuncSpec(s string) (FuncSpec, error) {
	pkg, name, ok := strings.Cut(s, "#")
	if !ok || pkg == "" || name == "" {
		return FuncSpec{}, fmt.Errorf("%q is not \"importpath#Name\"", s)
	}
	return FuncSpec{Pkg: pkg, Name: name}, nil
}

// parseFieldSpec parses "importpath#Type.Field".
func parseFieldSpec(s string) (FieldSpec, error) {
	pkg, rest, ok := strings.Cut(s, "#")
	if !ok || pkg == "" {
		return FieldSpec{}, fmt.Errorf("%q is not \"importpath#Type.Field\"", s)
	}
	typ, field, ok := strings.Cut(rest, ".")
	if !ok || typ == "" || field == "" {
		return FieldSpec{}, fmt.Errorf("%q is not \"importpath#Type.Field\"", s)
	}
	return FieldSpec{Pkg: pkg, Type: typ, Field: field}, nil
}

// Empty reports whether no sources or sinks are declared (so the analysis has
// nothing to do).
func (c Config) Empty() bool {
	return len(c.SourceFuncs) == 0 && len(c.SourceFields) == 0 && len(c.Sinks) == 0
}
