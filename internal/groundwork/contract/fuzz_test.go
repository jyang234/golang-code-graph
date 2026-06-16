package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzContractLoad pins that the boundary-contract loader never panics on
// arbitrary file contents — the diff that `groundwork diff` reports rides on it,
// and a crash on a malformed/truncated contract is a production-facing failure
// rather than the clean error Load is meant to return (including the empty/null
// guard). Seeded from the committed contract goldens.
func FuzzContractLoad(f *testing.F) {
	dir := filepath.Join("..", "..", "..", "testdata", "groundwork", "goldens")
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".contract.json") {
				continue
			}
			if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
				f.Add(b)
			}
		}
	}
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"service":"x"}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "contract.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Skip()
		}
		_, _ = Load(path) // must return an error, never panic, on any bytes
	})
}
