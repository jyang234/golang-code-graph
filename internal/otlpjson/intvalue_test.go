package otlpjson

import "testing"

// TestStrIntValueFailsClosed pins that an OTLP intValue is parsed as an integer
// and a malformed/null value is REFUSED (rendered ""), not accepted as a garbage
// attribute string. The value flows into the per-span attrs map and op keys, so a
// fail-open here would leak an unvalidated/garbage value into the canonical key.
func TestStrIntValueFailsClosed(t *testing.T) {
	cases := []struct {
		raw  string // raw JSON bytes of the intValue field
		want string
	}{
		{`"123"`, "123"}, // OTLP canonical: int64 as a JSON string
		{`123`, "123"},   // tolerate a bare JSON number
		{`"-7"`, "-7"},   // negative
		{`null`, ""},     // null is absent, not the literal "null"
		{`"1\"2"`, ""},   // not an integer → refused, not "1\"2"
		{`"abc"`, ""},    // garbage → refused
		{`""`, ""},       // empty → refused
	}
	for _, c := range cases {
		v := anyValue{IntValue: []byte(c.raw)}
		if got := v.str(); got != c.want {
			t.Errorf("str(intValue=%s) = %q, want %q", c.raw, got, c.want)
		}
	}
}
