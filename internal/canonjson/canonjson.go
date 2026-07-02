// Package canonjson is flowmap's single deterministic JSON serializer. Every
// gated artifact — boundary contracts and golden snapshots — is written through
// it, so that re-running a generator yields byte-identical output. That property
// is what the whole gating model rests on.
//
// Determinism rules, enforced here in one place:
//
//   - Object keys from Go maps are emitted in sorted order. encoding/json already
//     sorts string-keyed maps at every depth; struct fields keep declaration
//     order, which is itself deterministic and reads better than alphabetical.
//   - HTML escaping is disabled, so a placeholder value such as "<uuid>" survives
//     verbatim instead of becoming "\u003cuuid\u003e" — encoding/json escapes
//     <, >, and & to their \u00xx form by default, which would churn goldens and
//     make the literals unreadable in a diff.
//   - Output is indented with two spaces for reviewable diffs and terminated with
//     a trailing newline for clean git behavior.
//
// Determinism is also a property of the input: callers must keep volatile data
// (timestamps, counts, host identifiers) out of the values they hand to Marshal.
package canonjson

import (
	"bytes"
	"encoding/json"
)

// Marshal encodes v as canonical, deterministic, human-reviewable JSON.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
