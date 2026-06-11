package main

import (
	"regexp"

	"github.com/jyang234/golang-code-graph/internal/canonjson"
	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
)

var siteRe = regexp.MustCompile(`^(.+\.go):(\d+)$`)

// toSARIF renders findings as minimal SARIF 2.1.0 so violations land as
// inline annotations in PR review UIs — the witness line is where the
// reviewer's eyes already are. Findings whose To is an obligation site
// (file:line) get a physical location; graph-level findings are run-level
// results with the FQNs in the message.
func toSARIF(findings []fitness.Finding) ([]byte, error) {
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
	return canonjson.Marshal(map[string]any{
		"$schema": "https://json.schemastore.org/sarif-2.1.0.json",
		"version": "2.1.0",
		"runs": []map[string]any{{
			"tool":    map[string]any{"driver": map[string]any{"name": "groundwork", "informationUri": "https://github.com/jyang234/golang-code-graph"}},
			"results": results,
		}},
	})
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
