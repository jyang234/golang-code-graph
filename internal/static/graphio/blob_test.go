package graphio_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
)

// TestMethodNamedOutboundEdges: object-storage, cache, and non-HTTP RPC calls
// classified via classify.{objectStore,cache,rpc} render as typed
// `boundary:<kind> <Method>` edges (the operation is the callee method name),
// tiered like outbound-sync effects.
func TestMethodNamedOutboundEdges(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", "blobsvc")
	res, err := analyze.Analyze(dir)
	if err != nil {
		t.Fatalf("analyze blobsvc: %v", err)
	}
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range g.Edges {
		if label, ok := strings.CutPrefix(e.To, "boundary:"); ok {
			got[label] = true
		}
	}
	for _, want := range []string{
		"blob PutObject", "blob GetObject",
		"cache Get", "cache Set",
		"rpc Charge",
	} {
		if !got[want] {
			t.Errorf("want a boundary:%s edge; got %v", want, got)
		}
	}
}
