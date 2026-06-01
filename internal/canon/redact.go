package canon

import "regexp"

// Redaction value matchers (canon spec §3.2): generated values — UUIDs, numeric
// ids, timestamps — are replaced with a type placeholder so "an id was here"
// stays visible without the volatile value leaking into the golden.
var (
	uuidRe    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	numericRe = regexp.MustCompile(`^\d+$`)
	rfc3339Re = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)
)

// placeholder reports whether val looks like a generated value and, if so,
// returns its type placeholder. Replace, not drop, so the shape stays visible.
func placeholder(val string) (string, bool) {
	switch {
	case uuidRe.MatchString(val):
		return "<uuid>", true
	case rfc3339Re.MatchString(val):
		return "<ts>", true
	case numericRe.MatchString(val):
		return "<id>", true
	default:
		return "", false
	}
}
