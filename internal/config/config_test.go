package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExample(t *testing.T) {
	// The .flowmap.yaml from the worked example (artifacts §8): mostly defaults.
	const y = `
version: 1
classify:
  telemetry:  ["go.uber.org/zap"]
  busPublish: ["github.com/koalafi/eventbus#Publish"]
  busConsume: ["github.com/koalafi/eventbus#Subscribe"]
  db:         ["github.com/jackc/pgx/v5"]
`
	c, err := Load([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 1 {
		t.Errorf("version = %d, want 1", c.Version)
	}
	if !c.UsesDefaults() {
		t.Error("UsesDefaults should default to true when absent")
	}
	if c.CatchAllTier() != 3 {
		t.Errorf("CatchAllTier = %d, want 3", c.CatchAllTier())
	}
	if len(c.Classify.DB) != 1 || c.Classify.DB[0] != "github.com/jackc/pgx/v5" {
		t.Errorf("db hints = %v", c.Classify.DB)
	}
}

func TestLoadTierLayer(t *testing.T) {
	const y = `
useDefaults: false
catchAll: 4
rules:
  - match: { effect: mutate, identity: "*ledger*" }
    tier: 1
pins:
  - { identity: "*health#Ping", tier: 4 }
`
	c, err := Load([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if c.UsesDefaults() {
		t.Error("useDefaults:false not honored")
	}
	if c.CatchAllTier() != 4 {
		t.Errorf("CatchAllTier = %d, want 4", c.CatchAllTier())
	}
	if len(c.Rules) != 1 || c.Rules[0].Match.Identity != "*ledger*" || c.Rules[0].Tier != 1 {
		t.Errorf("rules = %+v", c.Rules)
	}
	if len(c.Pins) != 1 || c.Pins[0].Identity != "*health#Ping" || c.Pins[0].Tier != 4 {
		t.Errorf("pins = %+v", c.Pins)
	}
}

func TestLoadServiceAndHTTPHints(t *testing.T) {
	const y = `
version: 1
service: loansvc
classify:
  http: ["example.com/loansvc/internal/client#Call"]
`
	c, err := Load([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if c.Service != "loansvc" {
		t.Errorf("service = %q, want loansvc", c.Service)
	}
	if len(c.Classify.HTTP) != 1 || c.Classify.HTTP[0] != "example.com/loansvc/internal/client#Call" {
		t.Errorf("http hints = %v", c.Classify.HTTP)
	}
}

func TestLoadEmpty(t *testing.T) {
	c, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.UsesDefaults() || c.CatchAllTier() != 3 {
		t.Errorf("empty config should be all-defaults, got %+v", c)
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	if _, err := Load([]byte("nope: true\n")); err == nil {
		t.Fatal("expected error on unknown key")
	}
}

func TestLoadRejectsBadTier(t *testing.T) {
	if _, err := Load([]byte("rules:\n  - match: {effect: mutate}\n    tier: 9\n")); err == nil {
		t.Fatal("expected error on tier out of range")
	}
}

// A ratified seam (§8) must carry both a site to blind and a reason (the
// impeachment witness). Either missing is undisclosed drift, refused at load.
func TestLoadRejectsDeclaredBlindSpotMissingFields(t *testing.T) {
	if _, err := Load([]byte("static:\n  declaredBlindSpots:\n    - reason: stated\n")); err == nil {
		t.Fatal("expected error on a declared blind spot with no site")
	}
	if _, err := Load([]byte("static:\n  declaredBlindSpots:\n    - site: ex.com/svc.Seam\n")); err == nil {
		t.Fatal("expected error on a declared blind spot with no reason")
	}
	if _, err := Load([]byte("static:\n  declaredBlindSpots:\n    - site: ex.com/svc.Seam\n      reason: ratified witness\n")); err != nil {
		t.Fatalf("a site+reason seam must load: %v", err)
	}
}

// A declared entrypoint anchors on a specific function, so — like an obligation
// ref — both halves of the "import/path#Symbol" reference are required. A bare path
// (no symbol) is a typo refused at load, in either the callbacks or workers list.
func TestLoadEntrypoints(t *testing.T) {
	const doc = "entrypoints:\n" +
		"  callbacks:\n" +
		"    - ex.com/svc/internal/inbound#Handle\n" +
		"  workers:\n" +
		"    - ex.com/svc/internal/reconciler#Start\n"
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("valid entrypoints must load: %v", err)
	}
	if len(c.Entrypoints.Callbacks) != 1 || c.Entrypoints.Callbacks[0] != "ex.com/svc/internal/inbound#Handle" {
		t.Errorf("callbacks not parsed: %+v", c.Entrypoints.Callbacks)
	}
	if len(c.Entrypoints.Workers) != 1 || c.Entrypoints.Workers[0] != "ex.com/svc/internal/reconciler#Start" {
		t.Errorf("workers not parsed: %+v", c.Entrypoints.Workers)
	}

	if _, err := Load([]byte("entrypoints:\n  callbacks:\n    - ex.com/svc/inbound\n")); err == nil {
		t.Error("expected error on a callback reference with no symbol")
	}
	if _, err := Load([]byte("entrypoints:\n  workers:\n    - ex.com/svc/reconciler\n")); err == nil {
		t.Error("expected error on a worker reference with no symbol")
	}
}

func TestCanonDefaults(t *testing.T) {
	c, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.SalienceThreshold(); got != 2 {
		t.Errorf("default SalienceThreshold = %d, want 2 (warn)", got)
	}
}

func TestCanonSalienceTier(t *testing.T) {
	const y = `
canon:
  salienceTier: info
  attributeAllowlist: ["loan.product"]
  redactKeys: ["customer.email"]
`
	c, err := Load([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.SalienceThreshold(); got != 3 {
		t.Errorf("info SalienceThreshold = %d, want 3", got)
	}
	if len(c.Canon.AttributeAllowlist) != 1 || c.Canon.AttributeAllowlist[0] != "loan.product" {
		t.Errorf("attributeAllowlist = %v", c.Canon.AttributeAllowlist)
	}
	if len(c.Canon.RedactKeys) != 1 {
		t.Errorf("redactKeys = %v", c.Canon.RedactKeys)
	}
}

func TestCanonRejectsBadSalienceTier(t *testing.T) {
	if _, err := Load([]byte("canon:\n  salienceTier: loud\n")); err == nil {
		t.Fatal("expected error on bad salienceTier")
	}
}

func TestFanOutThreshold(t *testing.T) {
	def, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := def.FanOutThreshold(); got != 8 {
		t.Errorf("default FanOutThreshold = %d, want 8", got)
	}
	custom, err := Load([]byte("static:\n  highFanOutThreshold: 20\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := custom.FanOutThreshold(); got != 20 {
		t.Errorf("custom FanOutThreshold = %d, want 20", got)
	}
	if _, err := Load([]byte("static:\n  highFanOutThreshold: -1\n")); err == nil {
		t.Fatal("expected error on negative highFanOutThreshold")
	}
}

// TestDiscoverIsModuleBounded is the guard against the walk-up escaping the
// service module: a stray .flowmap.yaml above the module root (a parent module,
// the repo root, or a developer's $HOME) must never be discovered. Discovery
// stops at the first go.mod.
func TestDiscoverIsModuleBounded(t *testing.T) {
	base := t.TempDir()
	// Layout: base/.flowmap.yaml is ABOVE the module and must be ignored;
	// base/svc/go.mod is the module root; the search starts deep inside it.
	write(t, filepath.Join(base, FileName), "version: 1\n")
	svc := filepath.Join(base, "svc")
	nested := filepath.Join(svc, "internal", "flows")
	mkdirAll(t, nested)
	write(t, filepath.Join(svc, "go.mod"), "module example.com/svc\n")

	if dir, ok := Discover(nested); ok {
		t.Fatalf("Discover escaped the module root: found %q, want none (ancestor config must be ignored)", dir)
	}

	// With an in-module config at the module root, discovery finds it.
	write(t, filepath.Join(svc, FileName), "version: 1\n")
	dir, ok := Discover(nested)
	if !ok || dir != svc {
		t.Fatalf("Discover = (%q, %v), want (%q, true)", dir, ok, svc)
	}
}

// TestLoadDirMissingIsDefaults: an absent config is not an error — it yields the
// zero Config so defaults apply.
func TestLoadDirMissingIsDefaults(t *testing.T) {
	cfg, err := LoadDir(t.TempDir())
	if err != nil {
		t.Fatalf("LoadDir on a dir without %s: %v", FileName, err)
	}
	if cfg.SalienceThreshold() != 2 {
		t.Errorf("default SalienceThreshold = %d, want 2 (warn)", cfg.SalienceThreshold())
	}
}

// TestLoadDirSurfacesReadError: a present-but-unreadable config is a hard error,
// not a silent fall-through to defaults (which would gate against the wrong
// tiering). A directory at the config path forces a non-not-exist read error
// portably.
func TestLoadDirSurfacesReadError(t *testing.T) {
	dir := t.TempDir()
	mkdirAll(t, filepath.Join(dir, FileName)) // .flowmap.yaml exists but is a directory
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("expected a hard error reading an unreadable config, got nil (silently defaulted)")
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestLoadObligations(t *testing.T) {
	c, err := Load([]byte(`
obligations:
  - name: tx-must-close
    acquire: "example.com/svc/internal/store#BeginTx"
    release: ["example.com/svc/internal/store#Commit", "example.com/svc/internal/store#Rollback"]
  - name: audit-before-publish
    require: "example.com/svc/internal/audit#Write"
    before: "example.com/svc/internal/eventbus#Publish"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Obligations) != 2 {
		t.Fatalf("obligations = %d, want 2", len(c.Obligations))
	}
	if k := c.Obligations[0].Kind(); k != KindMustRelease {
		t.Errorf("rule 0 kind = %s, want must-release", k)
	}
	if k := c.Obligations[1].Kind(); k != KindMustPrecede {
		t.Errorf("rule 1 kind = %s, want must-precede", k)
	}
}

func TestLoadRejectsBadObligations(t *testing.T) {
	cases := map[string]string{
		"no name":        "obligations:\n  - acquire: \"p#A\"\n    release: [\"p#R\"]\n",
		"duplicate name": "obligations:\n  - name: r\n    acquire: \"p#A\"\n    release: [\"p#R\"]\n  - name: r\n    require: \"p#A\"\n    before: \"p#B\"\n",
		"mixed kinds":    "obligations:\n  - name: r\n    acquire: \"p#A\"\n    release: [\"p#R\"]\n    before: \"p#B\"\n",
		"neither kind":   "obligations:\n  - name: r\n",
		"empty release":  "obligations:\n  - name: r\n    acquire: \"p#A\"\n",
		"no before":      "obligations:\n  - name: r\n    require: \"p#A\"\n",
		"bare package":   "obligations:\n  - name: r\n    acquire: \"example.com/p\"\n    release: [\"p#R\"]\n",
		"empty symbol":   "obligations:\n  - name: r\n    acquire: \"p#\"\n    release: [\"p#R\"]\n",
	}
	for name, doc := range cases {
		if _, err := Load([]byte(doc)); err == nil {
			t.Errorf("%s: Load() = nil, want error", name)
		}
	}
}
