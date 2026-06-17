package policy

import (
	"strings"
	"testing"
)

// PC-3: an effect-ratchet allow entry with an empty target would prefix-match
// every write label — one entry silently disabling the whole ratchet, the
// inert-guardrail failure mode the load validator exists to catch.
func TestEffectRatchetEmptyTargetRejected(t *testing.T) {
	p := &Policy{Service: "svc", Version: 1, EffectRatchet: &EffectRatchet{
		Allow: []EffectException{{Target: "  ", Reason: "oops"}},
	}}
	if err := p.Validate(); err == nil {
		t.Fatal("an empty effect_ratchet allow target must fail validation")
	}
}

func TestEffectRatchetAllows(t *testing.T) {
	// A table-level target binds that table exactly, at an identifier boundary —
	// it must NOT silently also allow a new, distinct table whose name merely
	// extends it (the guardrail-defeating fail-open).
	table := &EffectRatchet{Allow: []EffectException{{Target: "db INSERT users"}}}
	tableCases := map[string]bool{
		"db INSERT users":       true,  // exact
		"db INSERT users_audit": false, // distinct table — must NOT be allowed
		"db INSERT userspace":   false, // identifier continues
		"db INSERT other":       false,
		"db DELETE users":       false, // different op
	}
	for label, want := range tableCases {
		if got := table.Allows(label); got != want {
			t.Errorf("table.Allows(%q) = %v, want %v", label, got, want)
		}
	}

	// An op-level target still binds every write of that op — the space after the
	// op is an identifier boundary, so the common "bless all INSERTs" entry works.
	op := &EffectRatchet{Allow: []EffectException{{Target: "db INSERT"}}}
	opCases := map[string]bool{
		"db INSERT users": true,
		"db INSERT audit": true,
		"db INSERT":       true,  // exact
		"db DELETE users": false, // different op
	}
	for label, want := range opCases {
		if got := op.Allows(label); got != want {
			t.Errorf("op.Allows(%q) = %v, want %v", label, got, want)
		}
	}

	var nilRatchet *EffectRatchet
	if nilRatchet.Allows("db INSERT x") {
		t.Error("a nil ratchet must allow nothing")
	}
}

// The coupling caution fires on exactly ONE config — effect ratchet gating with the
// blind-spot ratchet NOT gating (its only backstop against dynamic-laundering is
// off) — and is silent on every other. This is the single source both policy-check
// and fitness read, so the truth table here is the parity guard for both surfaces.
func TestEffectRatchetCouplingCaution(t *testing.T) {
	gating := func(g bool) *EffectRatchet { return &EffectRatchet{Gate: g} }
	bsGating := func(g bool) *BlindSpotRatchet { return &BlindSpotRatchet{Gate: g} }

	cases := []struct {
		name      string
		effect    *EffectRatchet
		blindSpot *BlindSpotRatchet
		wantFire  bool
	}{
		{"effect gates, blind-spot gates", gating(true), bsGating(true), false},
		{"effect gates, blind-spot observe-only", gating(true), bsGating(false), true},
		{"effect gates, blind-spot absent", gating(true), nil, true}, // absent ⇒ not gating
		{"effect observe-only", gating(false), bsGating(false), false},
		{"effect absent", nil, bsGating(false), false},
		{"both absent", nil, nil, false},
	}
	for _, c := range cases {
		p := &Policy{Service: "svc", Version: 1, EffectRatchet: c.effect, BlindSpotRatchet: c.blindSpot}
		got := p.EffectRatchetCouplingCaution()
		if (got != "") != c.wantFire {
			t.Errorf("%s: caution=%q, want fire=%v", c.name, got, c.wantFire)
		}
		if c.wantFire && !strings.Contains(got, "blind_spot_ratchet") {
			t.Errorf("%s: caution must name blind_spot_ratchet as the remedy; got %q", c.name, got)
		}
	}
}
