package main

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
)

// TestSARIFCarriesCaveats pins that the provenance caveats (substrate mismatch,
// reclaim, algorithm) reach the SARIF output. The --sarif path is what CI
// annotates PRs from; if the caveats were dropped there, an unsound-substrate
// fitness pass would show as a clean green run with no warning — laundering an
// unsound verdict (CLAUDE.md: never launder an unsound substrate into clean).
func TestSARIFCarriesCaveats(t *testing.T) {
	caveat := "substrate mismatch: policy built on vta, graph built with rta"
	b, err := toSARIF([]fitness.Finding{}, []string{caveat})
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if !strings.Contains(out, caveat) {
		t.Errorf("SARIF output dropped the provenance caveat; CI would show a clean run.\n%s", out)
	}
	if !strings.Contains(out, "toolExecutionNotifications") {
		t.Errorf("caveats should ride invocations.toolExecutionNotifications; got:\n%s", out)
	}
}
