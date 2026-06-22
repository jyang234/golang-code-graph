package boundary_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/boundary"
)

// TestMethodNamedKindsInContract is the regression guard for promoting object
// storage / cache / non-HTTP RPC to typed kinds: each must appear in the GATED
// contract as an external dependency of its kind (so the disclosure is not lost),
// with the method names as ops (no package-init/constructor leakage), and none may
// also linger as an ExternalBoundaryCall blind spot (no double-count).
func TestMethodNamedKindsInContract(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", "blobsvc")
	res, err := analyze.Analyze(dir)
	if err != nil {
		t.Fatalf("analyze blobsvc: %v", err)
	}
	c := boundary.Extract(res)

	byKind := map[string]*boundary.ExternalDep{}
	for i := range c.ExternalDeps {
		byKind[c.ExternalDeps[i].Kind] = &c.ExternalDeps[i]
	}
	want := map[string]struct {
		peer string
		ops  map[string]bool
	}{
		"blob":  {"example.com/blobsvc/blobstore", map[string]bool{"GetObject": true, "PutObject": true}},
		"cache": {"example.com/blobsvc/cacheclient", map[string]bool{"Get": true, "Set": true}},
		"rpc":   {"example.com/blobsvc/rpcclient", map[string]bool{"Charge": true}},
	}
	for kind, w := range want {
		d := byKind[kind]
		if d == nil {
			t.Errorf("%s call must appear as a %s external dependency; deps = %+v", kind, kind, c.ExternalDeps)
			continue
		}
		if d.Peer != w.peer {
			t.Errorf("%s peer = %q, want %q", kind, d.Peer, w.peer)
		}
		for _, op := range d.Ops {
			if !w.ops[op] {
				t.Errorf("unexpected %s op %q (package-init/constructor must not leak in); ops = %v", kind, op, d.Ops)
			}
		}
		if len(d.Ops) != len(w.ops) {
			t.Errorf("%s ops = %v, want keys %v", kind, d.Ops, w.ops)
		}
	}

	// Promotion must not leave any classified call as a blind spot too.
	for _, b := range c.BlindSpots {
		if b.Kind == "ExternalBoundaryCall" {
			t.Errorf("a classified outbound call must not also be an ExternalBoundaryCall blind spot: %+v", b)
		}
	}
}
