// Package config loads per-bundle okf.toml configuration. It holds data only;
// the mapping from config to per-rule severity/enablement lives in the rules
// package (which imports this one). Defaults here are the spec-aligned defaults
// described in docs/RULES.md and docs/okf.example.toml — a bundle tightens them.
package config

import (
	"fmt"
	"path"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config mirrors the sections of okf.toml.
type Config struct {
	Bundle      Bundle            `toml:"bundle"`
	Links       Links             `toml:"links"`
	Filenames   Filenames         `toml:"filenames"`
	Frontmatter Frontmatter       `toml:"frontmatter"`
	Citations   Citations         `toml:"citations"`
	Index       Index             `toml:"index"`
	Worklist    Worklist          `toml:"worklist"`
	QMD         QMD               `toml:"qmd"`
	Glossary    Glossary          `toml:"glossary"`
	Schema      Schema            `toml:"schema"`
	Banner      Banner            `toml:"banner"`
	Gaps        Gaps              `toml:"gaps"`
	Rules       map[string]string `toml:"rules"`

	// Path is the file this config was loaded from; empty when using defaults.
	Path string `toml:"-"`
}

type Bundle struct {
	Root     string   `toml:"root"`
	Reserved []string `toml:"reserved"`
}

type Links struct {
	// Style is the OKF102 concept-cross-link policy: relative|absolute|any.
	// "any" disables OKF102 (the spec-aligned default).
	Style string `toml:"style"`
	// AllowWikilinks toggles OKF101; false disallows [[wiki-links]].
	AllowWikilinks bool `toml:"allow_wikilinks"`
	// CheckBroken governs OKF202: off|info|warning|error. Defaults to "info"; a
	// bundle may escalate broken links to a hard failure (see docs/RULES.md).
	CheckBroken string `toml:"check_broken"`
	// CheckAnchors governs OKF203: off|info|warning|error. Defaults to "info". The
	// heading-anchor sibling of CheckBroken — CheckBroken validates a cross-link's
	// target *file*, CheckAnchors validates its #heading *fragment* against
	// ordinary (non-glossary) pages. A bundle may escalate dangling anchors to a
	// hard failure (sigma/ideas does, to match its retired sync/ gate).
	CheckAnchors string `toml:"check_anchors"`
}

type Filenames struct {
	Case     string `toml:"case"`     // OKF103 case convention, e.g. "kebab"
	Severity string `toml:"severity"` // OKF103 severity override
}

type Frontmatter struct {
	RequireDescription bool   `toml:"require_description"` // OKF107 enable
	RequireTimestamp   bool   `toml:"require_timestamp"`   // OKF104 presence
	TimestampFormat    string `toml:"timestamp_format"`    // OKF104 format: rfc3339|date
}

type Citations struct {
	Heading          string `toml:"heading"`            // OKF105 heading
	Style            string `toml:"style"`              // OKF105 style: numbered|footnote
	RequireWhenCited bool   `toml:"require_when_cited"` // OKF105
	CheckTargets     bool   `toml:"check_targets"`      // OKF206 enable
}

type Index struct {
	CheckSync                   bool `toml:"check_sync"`                    // OKF106 enable
	DescriptionsFromFrontmatter bool `toml:"descriptions_from_frontmatter"` // OKF106
}

type Worklist struct {
	Orphans string `toml:"orphans"` // OKF201 (dependency-free)
}

// QMD configures the optional, qmd-backed extension rules (OKFEXT-QMD-01/02).
// They require a fresh local qmd index (and the qmd binary on PATH) and are OFF
// unless Enabled is set, so the core of okf lint stays dependency-free.
type QMD struct {
	Enabled                bool    `toml:"enabled"`                  // master opt-in for OKFEXT-QMD-*
	Path                   string  `toml:"path"`                     // qmd binary; default "qmd" (resolved on PATH)
	NearDuplicates         string  `toml:"near_duplicates"`          // OKFEXT-QMD-01 severity
	NearDuplicateThreshold float64 `toml:"near_duplicate_threshold"` // OKFEXT-QMD-01
	Staleness              string  `toml:"staleness"`                // OKFEXT-QMD-02 severity
}

// Glossary configures the optional, opt-in glossary extension
// (OKFEXT-GLOSSARY-*). It designates one or more Markdown files as single-file
// glossaries whose entries are addressable by anchor. The convention comes from
// the domain-modeling CONTEXT-FORMAT, not the OKF spec, so every glossary rule
// is OFF unless Enabled is set — a bundle that doesn't opt in sees no new
// diagnostics. Per-rule severity lives in the [rules] map.
//
// The anchor-host file may equivalently be designated by an `areas.json` entry
// carrying `role: glossary` (see internal/areas); the bundle unions that marker
// with Files, still gated on Enabled. The role marker is the config contract
// okftool and okfpub share, replacing any hardwired anchor-host path.
type Glossary struct {
	Enabled bool     `toml:"enabled"` // master opt-in for all OKFEXT-GLOSSARY-* rules
	Files   []string `toml:"files"`   // globs; the declared glossary/anchor-host files
}

// Schema configures the optional frontmatter-schema extension (OKFEXT-SCHEMA-01).
// It points at a bundle's /schema.json — the closed, authoritative column
// declaration for an export database (sigma/ideas #117) — against which each
// mirrored file's frontmatter is validated. Non-spec and OFF unless Enabled, so a
// bundle that does not export to such a database sees no new diagnostics. The rule
// severity lives in the [rules] map like every other extension.
type Schema struct {
	Enabled bool   `toml:"enabled"` // master opt-in for OKFEXT-SCHEMA-*
	File    string `toml:"file"`    // bundle-root-relative schema.json; default "schema.json"
}

// Banner configures the generated-page disclaimer banner okfpub prepends as
// block-0 of every published page (sigma/ideas ADR-0015). Unlike the lint
// extensions it defaults ON: the mirror's contract is that every page is generated
// from the repo and rolled back on edit, and the banner is what makes that
// contract self-documenting on the page. A bundle that publishes a co-authored
// surface can disable it. Text is the notice shown; the per-page source deep-link
// is resolved by okfpub (internal/publish/source), not configured here.
type Banner struct {
	Enabled bool   `toml:"enabled"` // prepend the banner block (default true)
	Text    string `toml:"text"`    // the disclaimer notice shown on every page
}

// DefaultBannerText is the generated-page notice used when a bundle sets no custom
// [banner] text. It states the strict-mirror contract in one line, matching the
// production mirror's long-standing banner copy.
const DefaultBannerText = "🔄 Generated from the repo — edits here are rolled back on the next sync; edit the source file."

// Gaps configures defaults for `okftool gaps`. CLI flags override these; the
// config lets a bundle set its own defaults (e.g. depth = "neighborhood" when
// indirect bridges matter more than direct ones).
type Gaps struct {
	Depth        string   `toml:"depth"`         // direct|neighborhood
	Top          int      `toml:"top"`           // neighbors to consider
	MinSim       float64  `toml:"min_sim"`       // similarity floor
	ExcludeTypes []string `toml:"exclude_types"` // node types to skip
}

// Default returns the spec-aligned default configuration.
func Default() *Config {
	return &Config{
		Bundle: Bundle{
			Root:     ".",
			Reserved: []string{"index.md", "log.md"},
		},
		Links: Links{
			Style:          "any",
			AllowWikilinks: false,
			CheckBroken:    "info",
			CheckAnchors:   "info",
		},
		Filenames: Filenames{
			Case:     "kebab",
			Severity: "info",
		},
		Frontmatter: Frontmatter{
			RequireDescription: false,
			RequireTimestamp:   false,
			TimestampFormat:    "rfc3339",
		},
		Citations: Citations{
			Heading:          "# Citations",
			Style:            "numbered",
			RequireWhenCited: false,
			CheckTargets:     false,
		},
		Index: Index{
			CheckSync:                   true,
			DescriptionsFromFrontmatter: true,
		},
		Worklist: Worklist{
			Orphans: "info",
		},
		QMD: QMD{
			Enabled:                false,
			Path:                   "qmd",
			NearDuplicates:         "info",
			NearDuplicateThreshold: 0.85,
			Staleness:              "info",
		},
		Glossary: Glossary{
			Enabled: false,
			Files:   nil,
		},
		Schema: Schema{
			Enabled: false,
			File:    "schema.json",
		},
		Banner: Banner{
			Enabled: true,
			Text:    DefaultBannerText,
		},
		Gaps: Gaps{
			Depth:  "direct",
			Top:    10,
			MinSim: 0.4,
		},
		Rules: map[string]string{},
	}
}

// Load overlays the okf.toml at path onto the defaults. Keys absent from the
// file keep their default values. A nil/empty path returns the defaults.
func Load(path string) (*Config, error) {
	c := Default()
	if path == "" {
		return c, nil
	}
	if _, err := toml.DecodeFile(path, c); err != nil {
		return nil, err
	}
	if c.Rules == nil {
		c.Rules = map[string]string{}
	}
	c.Path = path
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

var severities = []string{"off", "info", "warning", "error"}

// Validate rejects unknown enum values so typos surface loudly instead of being
// silently ignored.
func (c *Config) Validate() error {
	checks := []struct {
		key, val string
		allowed  []string
	}{
		{"links.style", c.Links.Style, []string{"relative", "absolute", "any"}},
		{"links.check_broken", c.Links.CheckBroken, severities},
		{"links.check_anchors", c.Links.CheckAnchors, severities},
		{"filenames.case", c.Filenames.Case, []string{"kebab", "any"}},
		{"filenames.severity", c.Filenames.Severity, severities},
		{"frontmatter.timestamp_format", c.Frontmatter.TimestampFormat, []string{"rfc3339", "date"}},
		{"citations.style", c.Citations.Style, []string{"numbered", "footnote"}},
		{"gaps.depth", c.Gaps.Depth, []string{"direct", "neighborhood"}},
		{"worklist.orphans", c.Worklist.Orphans, severities},
		{"qmd.near_duplicates", c.QMD.NearDuplicates, severities},
		{"qmd.staleness", c.QMD.Staleness, severities},
	}
	for _, ck := range checks {
		if !oneOf(ck.val, ck.allowed) {
			return fmt.Errorf("okf.toml: %s = %q is not one of %s", ck.key, ck.val, strings.Join(ck.allowed, "|"))
		}
	}
	for id, sev := range c.Rules {
		if !oneOf(sev, severities) {
			return fmt.Errorf("okf.toml: [rules] %s = %q is not one of %s", id, sev, strings.Join(severities, "|"))
		}
	}
	for _, g := range c.Glossary.Files {
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("okf.toml: [glossary] files entry %q is not a valid glob: %w", g, err)
		}
	}
	if c.Schema.Enabled && strings.TrimSpace(c.Schema.File) == "" {
		return fmt.Errorf("okf.toml: [schema] enabled requires a non-empty file")
	}
	return nil
}

// IsGlossary reports whether the bundle-relative path (forward slashes) matches
// any declared [glossary] files glob. Parser/bundle and the glossary rules share
// this one definition of "is this a glossary file". It always returns false when
// the extension is disabled, so a bundle that hasn't opted in has no glossaries.
func (c *Config) IsGlossary(rel string) bool {
	if !c.Glossary.Enabled {
		return false
	}
	rel = path.Clean(strings.TrimPrefix(rel, "/"))
	for _, g := range c.Glossary.Files {
		if ok, _ := path.Match(g, rel); ok {
			return true
		}
	}
	return false
}

func oneOf(v string, allowed []string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// ReservedSet returns the reserved filenames as a set for quick membership tests.
func (c *Config) ReservedSet() map[string]bool {
	m := make(map[string]bool, len(c.Bundle.Reserved))
	for _, r := range c.Bundle.Reserved {
		m[r] = true
	}
	return m
}
