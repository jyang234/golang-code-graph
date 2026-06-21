package graphio

// Component BAND: the architectural ROLE a component plays — the lane a reviewer
// sorts a service into at a glance (transport / application / provisioning / storage
// / infrastructure / tests). It is the semantic, name-read sibling of the C3 rollup's
// topological facts: where the edges say who-calls-whom, the band says what-kind-of-
// thing. Read off the import path by classifyBand and surfaced as Component.Band so a
// grouped render can lane the component boxes.
//
// A BAND IS A VIEW, NEVER A GATE — the single load-bearing property of this axis.
// classifyBand computes no verdict; nothing PROVEN/VIOLATED/CANT-PROVE keys on it and
// no consumer should gate on it. THAT is what makes an (unsound by nature) name
// heuristic acceptable here: a misfiled band is a cosmetic nit, not an unsound result,
// so the prime directive's soundness-asymmetry — which binds verdicts — does not bind
// it. A name rule like this would be FORBIDDEN in the layering policy precisely because
// that one is a gate.
//
// NOT the layering layer, and the two must never be conflated:
//   - fitness.proposeLayers / policy.Layer is the ENFORCEMENT axis: a TOPOLOGICAL
//     call-rank (longest path in the package DAG) the layering policy checks call
//     DIRECTION against. It withdraws on a package cycle and auto-names "layer-N",
//     because for direction-checking the rank IS the content and the name is not.
//   - a BAND is the orthogonal SEMANTIC axis: the role read from the name, feeding the
//     human C3 grouping only. Where call-depth puts bootstrap (infra) and the domain
//     core at the same rank — topologically alike, only the name tells them apart — the
//     band keeps them in different lanes.
//
// So the type, the field, the constants, and this doc all say BAND, never LAYER, so a
// future reader cannot mistake the semantic view axis for the topological gate one.
//
// A service that needs its bands exact DECLARES them — policy.Layer is already that
// place — and a render can prefer the declared layer over classifyBand. graphio is the
// static front-end and cannot import policy, so that override is CALLER-supplied (the
// cmd layer that holds both passes the declared map into the render); graphio stays
// policy-agnostic. The convention default and the declared override are the same shape.

import "strings"

// Band values. Lexically separate from policy.Layer (the topological enforcement axis)
// on purpose: a band names a SEMANTIC role for the C3 view, never a call-rank.
const (
	// BandTransport is the edge-of-the-system lane: HTTP/gRPC handlers, servers,
	// gateways, webhooks — where a request enters or leaves over a wire protocol.
	BandTransport = "transport"
	// BandApplication is the domain/use-case core, and the DISCLOSED fallback for a
	// name that carries no role signal (see classifyBand) — never a silent guess.
	BandApplication = "application"
	// BandProvisioning is infrastructure-lifecycle code: cloud resource provisioning,
	// reconcilers, the AWS/SDK wrappers a provisioning flow leans on.
	BandProvisioning = "provisioning"
	// BandStorage is the persistence lane: stores, repositories, DAOs.
	BandStorage = "storage"
	// BandInfrastructure is the wiring/setup lane: bootstrap, config, dependency setup
	// (distinct from the composition ROOT, which is named by Role, not banded).
	BandInfrastructure = "infrastructure"
	// BandTests is test-support code (testutil and the like).
	BandTests = "tests"
)

// transportLeaf / storageLeaf / infraLeaf are matched on the package LEAF (its bare
// name); the broader provisioning/tests signals in classifyBand match the WHOLE path,
// so a sub-package of a provisioning parent (awsprovisioner/awsutil,
// provisioningoutbox/sourceid) inherits the band. The specific names are Go-service-
// conventional starting points — re-check and widen against your own fixtures; the
// idiom (leaf-match the precise lanes, path-match the broad ones) is the general part.
var (
	transportLeaf = map[string]bool{
		"api": true, "handler": true, "server": true, "delivery": true,
		"gateway": true, "rest": true, "grpc": true, "http": true,
		"transport": true, "webhook": true, "ingress": true,
	}
	storageLeaf = map[string]bool{
		"storage": true, "store": true, "repo": true,
		"repository": true, "persistence": true, "dao": true,
	}
	infraLeaf = map[string]bool{
		"bootstrap": true, "config": true, "wiring": true, "di": true, "setup": true,
	}
)

// classifyBand reads a component's BAND off its import path. Pure, deterministic, and
// ordered MOST-SPECIFIC-FIRST so a more precise signal wins over a broader one. It
// NEVER returns a root band — the composition root is named by Role (a graph fact, the
// SSA main package the rollup already carries), not by a name match — and it returns
// BandApplication for a name with no role signal: the disclosed fallback, not a guess.
//
// It is a CONVENTION READER, NOT A PROVER, and honest about it: the one class it cannot
// catch is a transport named for its domain (a consume loop named "subscriptions"),
// which falls to BandApplication. That is a cosmetic mislaning, never a hidden fact —
// the band emits no verdict, so the worst it does is draw a box in the wrong lane; how
// the component is ENTERED is a separate, entry-surface concern that does not key on it.
func classifyBand(pkg string) string {
	leaf := strings.ToLower(lastSegment(pkg))
	p := strings.ToLower(pkg)
	switch {
	case strings.HasPrefix(leaf, "test"):
		return BandTests
	case strings.Contains(p, "aws") || strings.Contains(p, "provision") || strings.Contains(p, "reconcil"):
		return BandProvisioning
	case transportLeaf[leaf] || strings.HasSuffix(leaf, "handler"):
		return BandTransport
	case storageLeaf[leaf]:
		return BandStorage
	case infraLeaf[leaf]:
		return BandInfrastructure
	default:
		return BandApplication
	}
}
