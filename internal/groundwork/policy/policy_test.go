package policy

import "testing"

func TestLoadValid(t *testing.T) {
	p, err := Load("../../../testdata/groundwork/policies/layeredsvc.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Service != "layeredsvc" {
		t.Errorf("service = %q, want layeredsvc", p.Service)
	}
	if got := p.LayerNames(); len(got) != 3 || got[0] != "handler" || got[2] != "store" {
		t.Errorf("LayerNames = %v, want [handler app store]", got)
	}
	if p.IOBudget == nil || p.IOBudget.MaxWritesPerRoute != 2 {
		t.Errorf("io budget = %v, want max 2", p.IOBudget)
	}
}

func TestLayerOfLongestPrefixWins(t *testing.T) {
	p := &Policy{
		Service: "s", Version: 1,
		Layers: []Layer{
			{Name: "app", Packages: []string{"example.com/s/internal"}},
			{Name: "store", Packages: []string{"example.com/s/internal/store"}},
		},
	}
	if got := p.LayerOf("example.com/s/internal/store"); got != "store" {
		t.Errorf("LayerOf(store pkg) = %q, want store (longest prefix)", got)
	}
	if got := p.LayerOf("example.com/s/internal/app"); got != "app" {
		t.Errorf("LayerOf(app pkg) = %q, want app", got)
	}
	if got := p.LayerOf("other.com/x"); got != "" {
		t.Errorf("LayerOf(unclaimed) = %q, want empty", got)
	}
}

func TestLayerRank(t *testing.T) {
	p := &Policy{Layers: []Layer{{Name: "a"}, {Name: "b"}, {Name: "c"}}}
	for i, name := range []string{"a", "b", "c"} {
		if r, ok := p.LayerRank(name); !ok || r != i {
			t.Errorf("LayerRank(%q) = (%d,%v), want (%d,true)", name, r, ok, i)
		}
	}
	if _, ok := p.LayerRank("missing"); ok {
		t.Error("LayerRank(missing) ok = true, want false")
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]*Policy{
		"no service":       {Version: 1},
		"no version":       {Service: "s"},
		"dup layer":        {Service: "s", Version: 1, Layers: []Layer{{Name: "a", Packages: []string{"p"}}, {Name: "a", Packages: []string{"q"}}}},
		"layer no package": {Service: "s", Version: 1, Layers: []Layer{{Name: "a"}}},
		"layering no layers": {
			Service: "s", Version: 1, Layering: &Layering{Roots: []string{"r"}},
		},
		"reach no name": {Service: "s", Version: 1, MustNotReach: []ReachRule{{From: []string{"a"}, To: []string{"b"}}}},
		"reach no from": {Service: "s", Version: 1, MustNotReach: []ReachRule{{Name: "r", To: []string{"b"}}}},
		"negative budget": {
			Service: "s", Version: 1, IOBudget: &IOBudget{MaxWritesPerRoute: -1},
		},
		"ratchet allow no site": {
			Service: "s", Version: 1,
			BlindSpotRatchet: &BlindSpotRatchet{Allow: []BlindSpotException{{Kind: "reflect", Reason: "r"}}},
		},
	}
	for name, p := range cases {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want error", name)
		}
	}
}

func TestBlindSpotRatchetAllows(t *testing.T) {
	var nilRatchet *BlindSpotRatchet
	if nilRatchet.Allows("reflect", "pkg.Fn") {
		t.Error("nil ratchet allowed a blind spot; it must allow nothing")
	}
	r := &BlindSpotRatchet{Allow: []BlindSpotException{
		{Site: "example.com/svc/internal/codec"},   // any kind, prefix
		{Kind: "HighFanOut", Site: "pkg.Dispatch"}, // kind-narrowed, exact
	}}
	if !r.Allows("reflect", "example.com/svc/internal/codec.Decode") {
		t.Error("prefix allow entry did not match")
	}
	if !r.Allows("HighFanOut", "pkg.Dispatch") {
		t.Error("exact kind+site allow entry did not match")
	}
	if r.Allows("reflect", "pkg.Dispatch") {
		t.Error("kind-narrowed entry matched a different kind")
	}
	if r.Allows("reflect", "example.com/svc/other.Fn") {
		t.Error("entry matched an unrelated site")
	}
}

func TestValidateAccepts(t *testing.T) {
	p := &Policy{
		Service: "s", Version: 1,
		Layers:   []Layer{{Name: "h", Packages: []string{"p/h"}}, {Name: "s", Packages: []string{"p/s"}}},
		Layering: &Layering{Roots: []string{"p"}},
		IOBudget: &IOBudget{MaxWritesPerRoute: 0},
	}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}
