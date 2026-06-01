// Package config loads and validates the per-service .flowmap.yaml document.
// Phase 1 models the tier layer — classification hints plus tier rules and pins.
// Later phases extend the struct with canonicalization knobs and per-flow
// declarations; unknown keys are rejected so a typo never silently disables a
// policy.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// Config mirrors .flowmap.yaml. Tier rules and pins are ordered slices, never
// maps, because map iteration order is non-deterministic and first-match wins.
type Config struct {
	Version  int           `yaml:"version"`
	Service  string        `yaml:"service"` // service name for the boundary contract; defaults to the module's last path segment
	Classify ClassifyHints `yaml:"classify"`

	// Tier layer (tier-map spec §3b).
	UseDefaults *bool  `yaml:"useDefaults"` // nil => true
	CatchAll    int    `yaml:"catchAll"`    // 0 => default 3
	Rules       []Rule `yaml:"rules"`
	Pins        []Pin  `yaml:"pins"`

	// Canonicalization layer (canon spec §4).
	Canon CanonConfig `yaml:"canon"`
}

// CanonConfig holds the policies the behavioral canonicalizer applies (canon
// spec §4). Every field defaults, so an empty document yields the spec's
// recommended defaults: retain tiers 1–2, the OTel-semconv attribute allowlist,
// and the built-in redaction matchers.
type CanonConfig struct {
	// SalienceTier is the minimum tier retained in the snapshot: "warn" keeps
	// tiers 1–2 (default), "info" keeps 1–3, "debug"/"all" keep everything. Spans
	// above the threshold are dropped and their survivors promoted.
	SalienceTier string `yaml:"salienceTier"`
	// AttributeAllowlist names extra attribute keys to retain in the snapshot.
	// By default the canonicalizer folds identity-bearing attributes into the op
	// key and keeps only a normalized db.statement, so the snapshot stays small;
	// listing a key here keeps it (after redaction) alongside the op.
	AttributeAllowlist []string `yaml:"attributeAllowlist"`
	// RedactKeys names retained attribute keys whose values are always replaced
	// with a type placeholder, on top of the built-in UUID / numeric-id /
	// timestamp value matchers. Only attributes that are retained (an allowlisted
	// key) are projected, so a redact key has effect only when it is also kept.
	RedactKeys []string `yaml:"redactKeys"`
}

// ClassifyHints name the libraries flowmap cannot infer: loggers, the bus client,
// the DB layer, and the outbound HTTP/RPC seam (tier-map spec §3a). Each entry is
// an import path, optionally narrowed to one symbol with "#Name" (e.g.
// "example.com/svc/internal/eventbus#Publish"); a bare path matches any call into
// that package.
type ClassifyHints struct {
	Telemetry  []string `yaml:"telemetry"`
	BusPublish []string `yaml:"busPublish"`
	BusConsume []string `yaml:"busConsume"`
	DB         []string `yaml:"db"`
	// HTTP names outbound HTTP/RPC seam functions. By convention the call's first
	// three string arguments are the peer, method, and route template, read by the
	// boundary extractor to name the external dependency.
	HTTP []string `yaml:"http"`
}

// Rule maps a feature pattern to a tier. An empty MatchSpec matches everything.
type Rule struct {
	Name  string    `yaml:"name,omitempty"`
	Match MatchSpec `yaml:"match"`
	Tier  int       `yaml:"tier"`
}

// MatchSpec constrains normalized features. Empty string / nil fields are
// unconstrained. Identity is an identity glob ('*' crosses separators).
type MatchSpec struct {
	Boundary   string `yaml:"boundary,omitempty"`
	Effect     string `yaml:"effect,omitempty"`
	Origin     string `yaml:"origin,omitempty"`
	Fallible   *bool  `yaml:"fallible,omitempty"`
	Concurrent *bool  `yaml:"concurrent,omitempty"`
	Identity   string `yaml:"identity,omitempty"`
}

// Pin forces a tier for symbols/events matching an identity glob, ahead of all
// rules.
type Pin struct {
	Identity string `yaml:"identity"`
	Tier     int    `yaml:"tier"`
}

// Load parses and validates a .flowmap.yaml document. Unknown keys are rejected.
// An empty document is valid and yields the zero config (all defaults).
func Load(b []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("flowmap config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.CatchAll < 0 || c.CatchAll > 4 {
		return fmt.Errorf("flowmap config: catchAll %d out of range 0..4", c.CatchAll)
	}
	for i, r := range c.Rules {
		if r.Tier < 1 || r.Tier > 4 {
			return fmt.Errorf("flowmap config: rules[%d].tier %d out of range 1..4", i, r.Tier)
		}
	}
	for i, p := range c.Pins {
		if p.Tier < 1 || p.Tier > 4 {
			return fmt.Errorf("flowmap config: pins[%d].tier %d out of range 1..4", i, p.Tier)
		}
	}
	if _, ok := salienceTiers[c.Canon.SalienceTier]; c.Canon.SalienceTier != "" && !ok {
		return fmt.Errorf("flowmap config: canon.salienceTier %q not one of warn|info|debug|all", c.Canon.SalienceTier)
	}
	return nil
}

// salienceTiers maps a salience name to the maximum tier retained in a snapshot.
var salienceTiers = map[string]int{"warn": 2, "info": 3, "debug": 4, "all": 4}

// SalienceThreshold is the maximum (least consequential) tier kept in the
// canonical snapshot; spans with a higher tier number are dropped and promoted.
// Absent in config => "warn" => 2 (canon spec §4).
func (c *Config) SalienceThreshold() int {
	if t, ok := salienceTiers[c.Canon.SalienceTier]; ok {
		return t
	}
	return 2
}

// UsesDefaults reports whether the built-in tier rules layer beneath user rules.
// Absent in config => true.
func (c *Config) UsesDefaults() bool { return c.UseDefaults == nil || *c.UseDefaults }

// CatchAllTier is the tier for operations that match no rule (default 3).
func (c *Config) CatchAllTier() int {
	if c.CatchAll == 0 {
		return 3
	}
	return c.CatchAll
}
