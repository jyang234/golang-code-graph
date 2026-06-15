package fitness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestGoldenSectionManifest is the regen ratchet (code-review finding):
// regen.sh rewrites the goldens wholesale, so an analyzer change that
// silently stops emitting facts is laundered into the committed files and
// ratified by the tests that compare against them — exactly how loansvc's
// effect_order section vanished. The section counts are pinned HERE, in a
// file regen.sh never touches: a count change is legitimate only as a
// deliberate edit to the manifest in the same commit, which puts the
// regression-vs-intended question in front of a reviewer instead of nobody.
func TestGoldenSectionManifest(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "testdata", "groundwork", "goldens")
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest map[string]map[string]int
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}

	// Every committed graph golden must be pinned: an unmanifested golden is
	// a golden with no ratchet.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" && len(e.Name()) > len(".graph.json") &&
			e.Name()[len(e.Name())-len(".graph.json"):] == ".graph.json" {
			if _, ok := manifest[e.Name()]; !ok {
				t.Errorf("golden %s has no manifest entry — pin its section counts", e.Name())
			}
		}
	}

	names := make([]string, 0, len(manifest))
	for name := range manifest {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		g := loadGraph(t, name)
		got := map[string]int{
			"nodes":        len(g.Nodes),
			"edges":        len(g.Edges),
			"blind_spots":  len(g.BlindSpots),
			"entrypoints":  len(g.Entrypoints),
			"obligations":  len(g.Obligations),
			"effect_order": len(g.EffectOrder),
			"frontier":     len(g.Frontier),
		}
		for section, want := range manifest[name] {
			if got[section] != want {
				t.Errorf("%s: %s has %d entries, manifest pins %d — if the change is intended, update goldens/manifest.json in the same commit; if not, an analyzer regression just tried to launder itself",
					name, section, got[section], want)
			}
		}
	}
}
