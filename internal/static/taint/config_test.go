package taint

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/config"
)

func TestFromConfig(t *testing.T) {
	cfg := &config.Config{Taint: config.TaintConfig{
		SourceFuncs:  []string{"example.com/svc#GetEmail"},
		SourceFields: []string{"example.com/svc#Recipient.Secret"},
		Sinks:        []string{"log#Info"},
	}}
	c, err := FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.SourceFuncs) != 1 || c.SourceFuncs[0] != (FuncSpec{Pkg: "example.com/svc", Name: "GetEmail"}) {
		t.Errorf("sourceFuncs = %+v", c.SourceFuncs)
	}
	if len(c.SourceFields) != 1 || c.SourceFields[0] != (FieldSpec{Pkg: "example.com/svc", Type: "Recipient", Field: "Secret"}) {
		t.Errorf("sourceFields = %+v", c.SourceFields)
	}
	if len(c.Sinks) != 1 || c.Sinks[0] != (FuncSpec{Pkg: "log", Name: "Info"}) {
		t.Errorf("sinks = %+v", c.Sinks)
	}
	if c.Empty() {
		t.Error("Empty() = true, want false")
	}
}

// A malformed spec is a load-time error, not a silently dropped entry.
func TestFromConfigRejectsMalformed(t *testing.T) {
	bad := []config.TaintConfig{
		{SourceFuncs: []string{"no-hash"}},
		{Sinks: []string{"#NoPkg"}},
		{SourceFields: []string{"example.com/svc#NoDot"}},
		{SourceFields: []string{"example.com/svc#Type."}},
	}
	for _, tc := range bad {
		if _, err := FromConfig(&config.Config{Taint: tc}); err == nil {
			t.Errorf("FromConfig(%+v) = nil error, want a parse error", tc)
		}
	}
}
