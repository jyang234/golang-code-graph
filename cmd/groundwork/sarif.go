package main

import (
	"regexp"
	"strconv"

	"github.com/jyang234/golang-code-graph/internal/canonjson"
	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
)

var siteRe = regexp.MustCompile(`^(.+\.go):(\d+)$`)

// toSARIF renders findings as minimal SARIF 2.1.0 so violations land as
// inline annotations in PR review UIs — the witness line is where the
// reviewer's eyes already are. Findings whose To is an obligation site
// (file:line) get a physical location; graph-level findings are run-level
// results with the FQNs in the message.
//
// caveats are the provenance disclosures (substrate/algorithm mismatch, reclaim)
// the human-readable path prints. They MUST ride the SARIF too — as
// invocation-level toolExecutionNotifications — or a fitness pass computed on an
// unsound substrate would annotate the PR as a clean green run, laundering an
// unsound verdict into "no findings" (CLAUDE.md: never launder an unsound
// substrate into a clean result).
func toSARIF(findings []fitness.Finding, caveats []string) ([]byte, error) {
	results := []map[string]any{}
	for _, f := range findings {
		level := "warning"
		if f.Severity == fitness.Violation {
			level = "error"
		}
		msg := f.Summary
		if f.From != "" {
			msg += " [" + f.From
			if f.To != "" {
				msg += " → " + f.To
			}
			msg += "]"
		}
		r := map[string]any{
			"ruleId": f.Rule, "level": level,
			"message": map[string]any{"text": msg},
		}
		if m := siteRe.FindStringSubmatch(f.To); m != nil {
			r["locations"] = []map[string]any{{
				"physicalLocation": map[string]any{
					"artifactLocation": map[string]any{"uri": m[1]},
					"region":           map[string]any{"startLine": atoiSafe(m[2])},
				},
			}}
		}
		results = append(results, r)
	}
	run := map[string]any{
		"tool":    map[string]any{"driver": map[string]any{"name": "groundwork", "informationUri": "https://github.com/jyang234/golang-code-graph"}},
		"results": results,
	}
	if len(caveats) > 0 {
		notes := make([]map[string]any, 0, len(caveats))
		for _, c := range caveats {
			notes = append(notes, map[string]any{
				"level":   "warning",
				"message": map[string]any{"text": c},
			})
		}
		// executionSuccessful stays true — a caveat is a provenance disclosure, not
		// a tool failure — but the warning-level notification surfaces it so the
		// substrate the verdict rests on is visible alongside the (possibly empty)
		// results, never silently dropped.
		run["invocations"] = []map[string]any{{
			"executionSuccessful":        true,
			"toolExecutionNotifications": notes,
		}}
	}
	return canonjson.Marshal(map[string]any{
		"$schema": "https://json.schemastore.org/sarif-2.1.0.json",
		"version": "2.1.0",
		"runs":    []map[string]any{run},
	})
}

// atoiSafe parses a SARIF line number, degrading to line 1 on a malformed or
// out-of-range value (SARIF region.startLine is 1-based) rather than silently
// wrapping to a negative number via unchecked n*10 multiplication.
func atoiSafe(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 1
	}
	return n
}
