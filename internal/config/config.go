// Package config loads per-bundle okf.toml configuration. It holds data only;
// the mapping from config to per-rule severity/enablement lives in the rules
// package (which imports this one). Defaults here are the spec-aligned defaults
// described in docs/RULES.md and docs/okf.example.toml — a bundle tightens them.
package config

import (
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
	// CheckBroken governs OKF202; hard-capped at "info", or "off" to disable.
	CheckBroken string `toml:"check_broken"`
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
	RequireWhenCited bool   `toml:"require_when_cited"` // OKF105
	CheckTargets     bool   `toml:"check_targets"`      // OKF206 enable
}

type Index struct {
	CheckSync                   bool `toml:"check_sync"`                    // OKF106 enable
	DescriptionsFromFrontmatter bool `toml:"descriptions_from_frontmatter"` // OKF106
}

type Worklist struct {
	Orphans                string  `toml:"orphans"`                  // OKF201
	NearDuplicates         string  `toml:"near_duplicates"`          // OKF203 (deferred)
	NearDuplicateThreshold float64 `toml:"near_duplicate_threshold"` // OKF203
	QMDStaleness           string  `toml:"qmd_staleness"`            // OKF204 (deferred)
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
			RequireWhenCited: false,
			CheckTargets:     false,
		},
		Index: Index{
			CheckSync:                   true,
			DescriptionsFromFrontmatter: true,
		},
		Worklist: Worklist{
			Orphans:                "info",
			NearDuplicates:         "info",
			NearDuplicateThreshold: 0.85,
			QMDStaleness:           "info",
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
	return c, nil
}

// ReservedSet returns the reserved filenames as a set for quick membership tests.
func (c *Config) ReservedSet() map[string]bool {
	m := make(map[string]bool, len(c.Bundle.Reserved))
	for _, r := range c.Bundle.Reserved {
		m[r] = true
	}
	return m
}
