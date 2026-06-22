package schemadrift

import (
	"fmt"
	"strings"
)

// Render formats a Report as a human-readable disclosure. It leads with the sound
// drift verdict, then the disclosures that scope it: the opaque-write count (the
// db-call frontier "no drift" is conditional on) and the advisory lower-bound. The
// output is a pure function of the Report, so it is deterministic.
func Render(name string, r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema-drift: %s\n", name)
	fmt.Fprintf(&b, "  migrations replayed: %d\n", r.Migrations)
	if len(r.LibraryOwned) > 0 {
		fmt.Fprintf(&b, "  library-owned (folded into schema): %s\n", strings.Join(r.LibraryOwned, ", "))
	}
	if len(r.Drift) == 0 {
		fmt.Fprintf(&b, "  DRIFT: none (among resolved writes)\n")
	} else {
		fmt.Fprintf(&b, "  DRIFT (%d) — code writes a table the schema does not define:\n", len(r.Drift))
		for _, d := range r.Drift {
			fmt.Fprintf(&b, "    - %s [%s] at %s\n", d.Table, strings.Join(d.Ops, ","), strings.Join(d.Sites, ", "))
		}
	}
	fmt.Fprintf(&b, "  opaque writes (db-call frontier, not checked): %d\n", r.OpaqueWrites)
	if len(r.Advisory) > 0 {
		fmt.Fprintf(&b, "  advisory (schema defines, code never resolves a write): %s\n", strings.Join(r.Advisory, ", "))
	}
	for _, c := range r.ParseCaveats {
		fmt.Fprintf(&b, "  caveat: %s\n", c)
	}
	return b.String()
}
