package flow

import (
	"fmt"
	"strings"
	"testing"
)

// tierTB records a Fatalf (which unwinds via panic, like testing.T's Goexit) so
// the test can assert resolveConfig rejects a bad tier for the right reason.
type tierTB struct{ fatal string }

func (t *tierTB) Helper()                   {}
func (t *tierTB) Logf(string, ...any)       {}
func (t *tierTB) Errorf(string, ...any)     {}
func (t *tierTB) Fatalf(f string, a ...any) { t.fatal = fmt.Sprintf(f, a...); panic(t) }

// TestFlowTierValidation pins H-14: the public Flow.Tier override is validated
// against the same warn|info|debug|all vocabulary config.Load enforces on the
// file path. An unknown name must fail loudly (not silently degrade to the warn
// default), and every valid name must pass — both paths, one vocabulary.
func TestFlowTierValidation(t *testing.T) {
	// Point config discovery at an empty dir so resolveConfig yields defaults and
	// never picks up a stray .flowmap.yaml from the working tree.
	empty := t.TempDir()

	for _, name := range []string{"warn", "info", "debug", "all"} {
		f := New("valid")
		f.configDir = empty
		f.tier = name
		rec := &tierTB{}
		cfg := f.resolveConfig(rec)
		if rec.fatal != "" {
			t.Errorf("Tier(%q): unexpected fatal %q — a valid tier must pass", name, rec.fatal)
		}
		if cfg.Canon.SalienceTier != name {
			t.Errorf("Tier(%q): override not applied (got %q)", name, cfg.Canon.SalienceTier)
		}
	}

	for _, bad := range []string{"Info", "trace", "al", "WARN", "verbose"} {
		f := New("invalid")
		f.configDir = empty
		f.tier = bad
		rec := &tierTB{}
		func() {
			defer func() { _ = recover() }() // Fatalf unwinds via panic
			f.resolveConfig(rec)
		}()
		if !strings.Contains(rec.fatal, "Tier") || !strings.Contains(rec.fatal, bad) {
			t.Errorf("Tier(%q): want a loud rejection naming the bad value, got fatal=%q", bad, rec.fatal)
		}
	}
}
