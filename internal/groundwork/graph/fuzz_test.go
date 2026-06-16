package graph

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzGraphLoad pins the contract that the graph loader — the single entry for
// every byte of untrusted flowmap output groundwork consumes — never panics on
// arbitrary input, and that a graph it accepts never panics the index builder
// the verdict engines run first. A malformed graph that crashes here is a
// production-facing crash, not a clean error. Seeded from the committed graph
// goldens (real flowmap output) so the mutation corpus surrounds valid graphs.
func FuzzGraphLoad(f *testing.F) {
	dir := filepath.Join("..", "..", "..", "testdata", "groundwork", "goldens")
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".graph.json") {
				continue
			}
			if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
				f.Add(b)
			}
		}
	}
	f.Add([]byte(`{"nodes":[]}`))
	f.Add([]byte(`{"nodes":[{"fqn":"a"}],"edges":[{"from":"a","to":"b"}]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		g, err := Load(bytes.NewReader(data))
		if err != nil {
			return
		}
		// A graph that decodes must not panic any downstream consumer; the index
		// builder (run by every verdict engine) is the first and most exposed.
		_ = NewIndex(g)
	})
}
