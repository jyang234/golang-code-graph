package chains

import (
	"fmt"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
)

// Render is the human-facing text for the whole report: one block per chain
// card, then the per-service disclosure of any dynamically-named bus effects the
// cards could not name.
func (r *Report) Render() string {
	if len(r.Cards) == 0 {
		return "no bus events in the loaded fleet — no chains to compose\n"
	}
	var b strings.Builder
	for i, c := range r.Cards {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(c.Render())
	}
	for _, name := range setutil.SortedKeys(r.Dynamic) {
		if n := r.Dynamic[name]; n > 0 {
			fmt.Fprintf(&b, "\n⚠️  %s has %d dynamically-named bus effect(s) — events it cannot name statically are absent from every card above\n", name, n)
		}
	}
	return b.String()
}

// Render is the chain card for one event: the proven producer link(s), the
// assumed broker link, and the proven consumer link(s), each labeled, with an
// explicit note when the chain is open at one end.
func (c Card) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "chain: %s\n", c.Event)

	renderEnds := func(role string, ends []End) {
		for _, e := range ends {
			fmt.Fprintf(&b, "  [%s] %s — %s\n", Proven, role, e.Service)
			for _, fn := range e.Fns {
				fmt.Fprintf(&b, "    @ %s\n", short(fn))
			}
			for _, f := range e.Facts {
				fmt.Fprintf(&b, "    - %s\n", f)
			}
			if len(e.Facts) == 0 {
				fmt.Fprintf(&b, "    - (no obligations or effect_order facts proven at this handler)\n")
			}
		}
	}

	renderEnds("producer", c.Producers)
	b.WriteString(c.Broker.render())
	renderEnds("consumer", c.Consumers)

	if c.Open != "" {
		fmt.Fprintf(&b, "  ⚠️  %s\n", c.Open)
	}
	return b.String()
}

// render is the assumed broker link. The values are always printed; the warrant
// is not assumed — an unsigned block is flagged, never passed off as warranted.
func (l BrokerLink) render() string {
	if l.Undeclared {
		if len(l.Unselected) > 0 {
			// Brokers were declared, but none is named "bus": disclose which, so
			// this does not read as "no broker configured" when one nearly was.
			return fmt.Sprintf("  [%s] broker — %d declared (%s) but none named \"bus\": name one \"bus\" to print it as this link\n",
				Assumed, len(l.Unselected), strings.Join(l.Unselected, ", "))
		}
		return fmt.Sprintf("  [%s] broker — undeclared: this cross-service link is unprovable by static analysis; declare a `brokers` block in policy to print it\n", Assumed)
	}
	warrant := "⚠️  UNSIGNED (values declared, pending human sign-off)"
	if l.Signed {
		warrant = "signed by " + l.Decl.SignedBy
	}
	var fields []string
	add := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			fields = append(fields, k+": "+v)
		}
	}
	add("transport", l.Decl.Transport)
	add("delivery", l.Decl.Delivery)
	add("ordered", l.Decl.Ordered)
	add("consumers", l.Decl.Consumers)
	add("dedup", l.Decl.Dedup)

	var b strings.Builder
	fmt.Fprintf(&b, "  [%s] broker — %s   %s\n", Assumed, l.Name, warrant)
	if len(fields) > 0 {
		fmt.Fprintf(&b, "    %s\n", strings.Join(fields, "; "))
	}
	return b.String()
}
