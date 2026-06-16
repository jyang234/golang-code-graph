package policy

import "testing"

// TestBrokerBlock: the broker declaration loads, exposes its signed/unsigned
// state, and rejects values outside each closed-vocabulary field — a typo'd
// guarantee must not read on a fault card as a real (false) promise.
func TestBrokerBlock(t *testing.T) {
	p, err := Load("../../../testdata/groundwork/policies/bus-brokers.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b, ok := p.Brokers["bus"]
	if !ok {
		t.Fatalf("no bus broker in %v", p.Brokers)
	}
	if b.Delivery != "at-least-once" || b.Ordered != "false" {
		t.Errorf("broker = %+v", b)
	}
	if b.Signed() {
		t.Error("the fixture block is intentionally unsigned (no warrant yet)")
	}
	b.SignedBy = "John"
	if !b.Signed() {
		t.Error("a block with signed_by must read as signed")
	}

	base := &Policy{Service: "s", Version: 1}
	for _, bad := range []Broker{
		{Delivery: "atleastonce"}, // not in the delivery vocabulary
		{Ordered: "kinda"},        // not false|total|per-key:*
		{Consumers: "mostly"},     // not idempotent|not-idempotent
	} {
		p := *base
		p.Brokers = map[string]Broker{"bus": bad}
		if err := p.Validate(); err == nil {
			t.Errorf("Validate accepted an out-of-vocabulary broker field: %+v", bad)
		}
	}
	// per-key:<key> and total are valid ordered values.
	for _, ok := range []string{"false", "total", "per-key:account_id"} {
		p := *base
		p.Brokers = map[string]Broker{"bus": {Ordered: ok}}
		if err := p.Validate(); err != nil {
			t.Errorf("Validate rejected valid ordered %q: %v", ok, err)
		}
	}
}

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
		"pass no name": {
			Service: "s", Version: 1,
			MustPassThrough: []PassRule{{From: []string{"a"}, To: []string{"b"}, Through: []string{"c"}}},
		},
		"pass no through": {
			Service: "s", Version: 1,
			MustPassThrough: []PassRule{{Name: "r", From: []string{"a"}, To: []string{"b"}}},
		},
		"dup pass rule name": {
			Service: "s", Version: 1,
			MustPassThrough: []PassRule{
				{Name: "r", From: []string{"a"}, To: []string{"b"}, Through: []string{"c"}},
				{Name: "r", From: []string{"x"}, To: []string{"y"}, Through: []string{"z"}},
			},
		},
		"selector in reach to": {
			Service: "s", Version: 1,
			MustNotReach: []ReachRule{{Name: "r", From: []string{"a"}, To: []string{EntrypointSelector}}},
		},
		"selector in pass through": {
			Service: "s", Version: 1,
			MustPassThrough: []PassRule{{Name: "r", From: []string{"a"}, To: []string{"b"}, Through: []string{EntrypointSelector}}},
		},
		"pass allow both empty": {
			Service: "s", Version: 1,
			MustPassThrough: []PassRule{{Name: "r", From: []string{"a"}, To: []string{"b"}, Through: []string{"c"},
				Allow: []Exception{{Reason: "no sides"}}}},
		},
		// An all-empty layering exception prefix-matches every edge and would
		// silently disable the whole invariant.
		"layering allow both empty": {
			Service: "s", Version: 1,
			Layers:   []Layer{{Name: "a", Packages: []string{"p"}}},
			Layering: &Layering{Allow: []Exception{{Reason: "tbd"}}},
		},
		// "" prefix-matches everything; an empty element makes a rule (or a
		// layer's package claim) mean something other than what it says.
		"empty pattern in reach from": {
			Service: "s", Version: 1,
			MustNotReach: []ReachRule{{Name: "r", From: []string{""}, To: []string{"b"}}},
		},
		"empty pattern in reach to": {
			Service: "s", Version: 1,
			MustNotReach: []ReachRule{{Name: "r", From: []string{"a"}, To: []string{" "}}},
		},
		"empty pattern in concurrent to": {
			Service: "s", Version: 1,
			NoConcurrentReach: []ConcurrentRule{{Name: "r", To: []string{""}}},
		},
		"empty pattern in pass through": {
			Service: "s", Version: 1,
			MustPassThrough: []PassRule{{Name: "r", From: []string{"a"}, To: []string{"b"}, Through: []string{""}}},
		},
		"empty layer package": {
			Service: "s", Version: 1,
			Layers: []Layer{{Name: "a", Packages: []string{""}}},
		},
	}
	for name, p := range cases {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want error", name)
		}
	}
}

func TestPassRuleAllowed(t *testing.T) {
	r := &PassRule{Allow: []Exception{
		{From: "example.com/svc.main", Reason: "composition root"},     // any target
		{From: "pkg.Healthz", To: "boundary:db SELECT health"},         // exact pair
		{To: "boundary:db SELECT version", Reason: "version endpoint"}, // any source
	}}
	if !r.Allowed("example.com/svc.main", "boundary:db INSERT x") {
		t.Error("from-only entry must match any target")
	}
	if !r.Allowed("pkg.Healthz", "boundary:db SELECT health") {
		t.Error("exact pair did not match")
	}
	if r.Allowed("pkg.Healthz", "boundary:db INSERT x") {
		t.Error("pair entry matched a different target")
	}
	if !r.Allowed("pkg.Other", "boundary:db SELECT version") {
		t.Error("to-only entry must match any source")
	}
	if r.Allowed("pkg.Other", "boundary:db DELETE users") {
		t.Error("unrelated pair matched")
	}

	// Identifier-boundary matching, not bare strings.HasPrefix: an allow for a
	// write to "users" must NOT silently suppress a bypass writing a different
	// table whose label merely shares that prefix ("users_audit"). A bare prefix
	// here is a false SATISFIED on the must_pass_through gate.
	rb := &PassRule{Allow: []Exception{
		{From: "svc/app.GetUser", To: "boundary:db INSERT users"},
	}}
	if !rb.Allowed("svc/app.GetUser", "boundary:db INSERT users") {
		t.Error("exact allow did not match itself")
	}
	if rb.Allowed("svc/app.GetUserAvatar", "boundary:db INSERT users") {
		t.Error("source prefix split an identifier: GetUserAvatar must not match an allow for GetUser")
	}
	if rb.Allowed("svc/app.GetUser", "boundary:db INSERT users_audit") {
		t.Error("target prefix split an identifier: users_audit must not match an allow for users")
	}
	// A true boundary extension (a member under the allowed prefix) still binds.
	if !rb.Allowed("svc/app.GetUser", "boundary:db INSERT users WHERE id=$1") {
		t.Error("identifier-boundary extension of the allowed target should still match")
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
		Layers: []Layer{{Name: "h", Packages: []string{"p/h"}}, {Name: "s", Packages: []string{"p/s"}}},
		// A one-sided exception is the documented shape: the declared side
		// narrows the match, the empty side means "any".
		Layering: &Layering{Roots: []string{"p"}, Allow: []Exception{{From: "p/h", Reason: "reviewed"}}},
		IOBudget: &IOBudget{MaxWritesPerRoute: 0},
	}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}
