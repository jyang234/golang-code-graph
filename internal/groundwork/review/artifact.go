// Package review computes the deterministic MR review artifact from a base graph
// and a branch graph. It is the answer to the reviewer-comprehension problem of
// 100%-agent development: instead of trusting the agent's prose about its own
// change, the reviewer reads a verdict computed *from* the code's structure,
// which the agent cannot embellish.
//
// The artifact is a pure function of (policy, base graph, branch graph). Its
// digest lets a verifier recompute it from the source graphs and prove it was
// neither hand-edited (TAMPERED) nor computed against different code (STALE) — but
// the digest is NOT the security anchor: unforgeability comes from a trusted party
// recomputing the artifact from CI-generated graphs, never from the number
// itself. See docs/groundwork/pressure-test.md.
package review

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/canonjson"
)

// Verdict is the three-valued top-line result. The split between
// StructurallyClear and NoStructuralSignal is load-bearing: it stops a reviewer
// mistaking "the graph has nothing to say" (a body-only change, where logic bugs
// live) for "the graph says it is fine".
type Verdict string

const (
	// Block: a new invariant violation or a breaking contract change.
	Block Verdict = "BLOCK"
	// StructurallyClear: the structure changed but no invariant broke. Explicitly
	// NOT a logic or test sign-off.
	StructurallyClear Verdict = "STRUCTURALLY-CLEAR"
	// NoStructuralSignal: the graph is identical base-to-branch (a body-only
	// change); the graph abstains and says so.
	NoStructuralSignal Verdict = "NO-STRUCTURAL-SIGNAL"
)

// Shape is the reviewer's first triage: how far the change reaches structurally.
type Shape string

const (
	BodyOnly     Shape = "body-only"
	Localized    Shape = "localized"
	CrossPackage Shape = "cross-package"
	Broad        Shape = "broad"
)

// Artifact is the computed review. Every field is derived from the graphs; none
// is author-supplied prose. Digest is sha256 over the canonical encoding of all
// other fields.
type Artifact struct {
	Service       string           `json:"service"`
	Verdict       Verdict          `json:"verdict"`
	Shape         Shape            `json:"shape"`
	Touches       []PkgDelta       `json:"touches,omitempty"`
	NewViolations []Violation      `json:"new_violations,omitempty"`
	Contract      []ContractChange `json:"contract_changes,omitempty"`
	Effects       []EffectChange   `json:"io_effects,omitempty"`
	// RouteIO attributes write-surface movement to routes; NewWriteTargets is
	// the effect ratchet's drift (write labels new to the whole graph).
	RouteIO         []RouteIODelta `json:"route_io_deltas,omitempty"`
	NewWriteTargets []string       `json:"new_write_targets,omitempty"`
	Reach           []string       `json:"reachable_from,omitempty"`
	NewCautions     []Violation    `json:"new_cautions,omitempty"`
	// StandingCautions are the branch's cautions that hold identically on base
	// and branch — the steady-state "the graph cannot prove this" disclosures the
	// base→branch delta suppresses (R1). The load-bearing case is an io_budget
	// that is unenforceable on both sides (storage is "db call"): NewCautions
	// never lists it, so without this absolute section a green gate hides it. It
	// is non-blocking, exactly like NewCautions — visibility at the gate, not a
	// new failure.
	StandingCautions []Violation      `json:"standing_cautions,omitempty"`
	NewBlindSpots    []BlindSpotDelta `json:"new_blind_spots,omitempty"`
	DBLabelDrift     *DBLabelDrift    `json:"db_label_drift,omitempty"`
	// Algo and Caveats record the call-graph substrate the judged BRANCH graph
	// was built on (rta|vta|cha) and its soundness/precision notes — baked into
	// the digest so the artifact SELF-CERTIFIES "this verdict was computed on
	// vta-refined-over-rta", rather than leaving a reviewer to guess. A
	// base/branch substrate mismatch is disclosed as a synthesized caveat. Empty
	// when the graphs predate provenance recording (R3).
	Algo    string   `json:"algo,omitempty"`
	Caveats []string `json:"caveats,omitempty"`
	Digest  string   `json:"digest"`
}

// DBLabelDrift reports DB write-label fidelity eroding base→branch: the count of
// DB boundary-effect edges whose SQL verb the labeler could not read ("db call"
// or a bare method name, built from non-constant SQL). The io_budget, triage
// --table, and effect-ratchet write surfaces all read such an effect as zero
// writes, so a base→branch INCREASE is protection silently shrinking — a single
// point of failure for the whole write-surface family (F2). It is disclosed, not
// gated: reported only when the count grew, and it does not Block on its own.
type DBLabelDrift struct {
	Base   int `json:"base"`
	Branch int `json:"branch"`
}

// PkgDelta is the node add/remove count for one touched package.
type PkgDelta struct {
	Package      string `json:"package"`
	NodesAdded   int    `json:"nodes_added,omitempty"`
	NodesRemoved int    `json:"nodes_removed,omitempty"`
}

// Violation is a newly-introduced fitness finding (a violation or a caution),
// carrying the exact edge it fires on. Detail is presentation-only evidence (a
// witness path); it is excluded from the base-vs-branch finding key.
type Violation struct {
	Rule    string `json:"rule"`
	Summary string `json:"summary"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// ContractChange is one movement of the inter-service surface (entrypoints, bus
// publish/consume, outbound dependencies). A removal is breaking.
type ContractChange struct {
	Op       string `json:"op"` // "+" added, "-" removed
	Surface  string `json:"surface"`
	Name     string `json:"name"`
	Breaking bool   `json:"breaking,omitempty"`
}

// EffectChange is one external I/O effect the MR adds or removes, including the
// service's own DB writes (which are I/O but not inter-service contract).
type EffectChange struct {
	Op     string `json:"op"`
	Effect string `json:"effect"`
	Write  bool   `json:"write,omitempty"`
}

// BlindSpotDelta is one newly-introduced gap in the graph's knowledge — the
// blind-spot ratchet's unit of drift. Identity is (kind, site); Detail is
// presentation only and never part of the base-vs-branch key, so a re-worded
// disclosure does not resurface as "new".
type BlindSpotDelta struct {
	Kind   string `json:"kind"`
	Site   string `json:"site"`
	Detail string `json:"detail,omitempty"`
}

// digestOf returns the sha256 of a's canonical encoding with the Digest field
// cleared — the structural content the digest commits to.
func digestOf(a Artifact) string {
	a.Digest = ""
	return canonicalDigest(a)
}

// canonicalDigest is the shared digest primitive: sha256 over the canonical JSON
// encoding of v. Callers clear any self-referential digest field on v first.
func canonicalDigest(v any) string {
	b, err := canonjson.Marshal(v)
	if err != nil {
		// canonjson only fails on unencodable values; our artifacts have none.
		panic("groundwork/review: marshal for digest: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Marshal renders the artifact as canonical JSON (the form a verifier reads).
func (a Artifact) Marshal() ([]byte, error) { return canonjson.Marshal(a) }

// LoadArtifact decodes an artifact from JSON. It rejects unknown fields so a
// doctored artifact with extra keys cannot slip through.
func LoadArtifact(path string) (Artifact, error) {
	f, err := os.Open(path)
	if err != nil {
		return Artifact{}, err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var a Artifact
	if err := dec.Decode(&a); err != nil {
		return Artifact{}, fmt.Errorf("%s: %w", path, err)
	}
	return a, nil
}

// Render is the human-facing text form — what a reviewer reads in seconds before
// opening the diff.
func (a Artifact) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# MR structural review — %s\n", a.Verdict)
	fmt.Fprintf(&b, "digest %s · recompute to verify (deterministic; not author-editable)\n", short(a.Digest))
	b.WriteString(renderProvenance(a.Algo, a.Caveats))
	b.WriteString("\n")

	if a.Verdict == NoStructuralSignal {
		b.WriteString("The call graph is identical base-to-branch: this is a body-only change.\n")
		b.WriteString("The graph abstains — it has no structural signal here. This is NOT a\n")
		b.WriteString("logic or test sign-off; it is exactly where logic review matters most.\n")
		// A standing caution is an absolute fact about the (identical) graph, not
		// a statement about this change — so it must survive the abstain path. This
		// is the artifact-level half of R1: a body-only change against an
		// unenforceable budget still discloses it.
		if s := renderStandingCautions(a.StandingCautions); s != "" {
			b.WriteString("\n")
			b.WriteString(s)
		}
		return b.String()
	}

	fmt.Fprintf(&b, "Shape: %s\n", a.Shape)
	if len(a.Touches) > 0 {
		parts := make([]string, len(a.Touches))
		for i, t := range a.Touches {
			parts[i] = t.String()
		}
		fmt.Fprintf(&b, "Touches: %s\n", strings.Join(parts, ", "))
	}
	b.WriteString("\n")

	if len(a.NewViolations) > 0 {
		fmt.Fprintf(&b, "⛔ Introduces %d invariant violation(s)\n", len(a.NewViolations))
		for _, v := range a.NewViolations {
			fmt.Fprintf(&b, "- %s — %s\n", v.Rule, v.Summary)
			if v.From != "" {
				fmt.Fprintf(&b, "  - %s\n", edge(v))
			}
			if v.Detail != "" {
				fmt.Fprintf(&b, "  - via %s\n", v.Detail)
			}
		}
		b.WriteString("\n")
	}

	if len(a.Contract) > 0 {
		breaking := anyBreaking(a.Contract)
		kind := "additive"
		if breaking {
			kind = "BREAKING"
		}
		fmt.Fprintf(&b, "🔌 External contract changed (%s)\n", kind)
		for _, c := range a.Contract {
			fmt.Fprintf(&b, "- %s %s %s\n", c.Op, c.Surface, c.Name)
		}
		b.WriteString("\n")
	}

	if len(a.Effects) > 0 {
		fmt.Fprintf(&b, "💾 External I/O effects changed (%d)\n", len(a.Effects))
		for _, e := range a.Effects {
			tag := ""
			if e.Write {
				tag = " (write)"
			}
			fmt.Fprintf(&b, "- %s %s%s\n", e.Op, e.Effect, tag)
		}
		b.WriteString("\n")
	}

	if len(a.NewWriteTargets) > 0 {
		fmt.Fprintf(&b, "🧾 %d new external write target(s) — ratcheted surface growth\n", len(a.NewWriteTargets))
		for _, t := range a.NewWriteTargets {
			fmt.Fprintf(&b, "- %s\n", t)
		}
		b.WriteString("\n")
	}

	if a.DBLabelDrift != nil {
		fmt.Fprintf(&b, "🔎 DB write-label fidelity dropped: %d → %d unclassified DB effect(s)\n", a.DBLabelDrift.Base, a.DBLabelDrift.Branch)
		b.WriteString("  more mutations now flow through non-constant SQL the io_budget, triage\n")
		b.WriteString("  --table, and effect ratchet all read as zero writes — the write surface is\n")
		b.WriteString("  going blind here, independent of any rule.\n\n")
	}

	if len(a.RouteIO) > 0 {
		b.WriteString(renderRouteIO(a.RouteIO))
		b.WriteString("\n")
	}

	if len(a.Reach) > 0 {
		fmt.Fprintf(&b, "🌐 Reachable from %d existing entrypoint(s)\n", len(a.Reach))
		for _, r := range a.Reach {
			fmt.Fprintf(&b, "- %s\n", r)
		}
		b.WriteString("\n")
	}

	if len(a.NewCautions) > 0 {
		fmt.Fprintf(&b, "⚠️  %d new caution(s) — the graph cannot prove these\n", len(a.NewCautions))
		for _, c := range a.NewCautions {
			fmt.Fprintf(&b, "- %s — %s\n", c.Rule, c.Summary)
		}
		b.WriteString("\n")
	}

	if s := renderStandingCautions(a.StandingCautions); s != "" {
		b.WriteString(s)
		b.WriteString("\n")
	}

	if len(a.NewBlindSpots) > 0 {
		fmt.Fprintf(&b, "🕳️  %d new blind spot(s) — the graph's knowledge of this code shrank\n", len(a.NewBlindSpots))
		for _, s := range a.NewBlindSpots {
			fmt.Fprintf(&b, "- %s %s", s.Kind, s.Site)
			if s.Detail != "" {
				fmt.Fprintf(&b, " — %s", s.Detail)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// renderStandingCautions renders the absolute standing-caution section, or ""
// when there are none. It is the shared form for the review artifact and the
// pre-flight gate so the two surfaces word the disclosure identically. Standing
// cautions are non-blocking; the framing says so, so a reviewer never reads this
// as a failure reason.
func renderStandingCautions(cs []Violation) string {
	if len(cs) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️  %d standing caution(s) — unchanged by this MR, still unproven (non-blocking)\n", len(cs))
	for _, c := range cs {
		fmt.Fprintf(&b, "- %s — %s\n", c.Rule, c.Summary)
	}
	return b.String()
}

// String renders a package's node delta, e.g. "handler(+1)" or "store(+2,-1)".
func (p PkgDelta) String() string {
	name := p.Package
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	switch {
	case p.NodesRemoved == 0:
		return fmt.Sprintf("%s(+%d)", name, p.NodesAdded)
	case p.NodesAdded == 0:
		return fmt.Sprintf("%s(-%d)", name, p.NodesRemoved)
	default:
		return fmt.Sprintf("%s(+%d,-%d)", name, p.NodesAdded, p.NodesRemoved)
	}
}

func anyBreaking(cs []ContractChange) bool {
	for _, c := range cs {
		if c.Breaking {
			return true
		}
	}
	return false
}

func edge(v Violation) string {
	if v.To != "" {
		return v.From + " → " + v.To
	}
	return v.From
}

func short(digest string) string {
	if len(digest) > 16 {
		return digest[:16] + "…"
	}
	return digest
}
