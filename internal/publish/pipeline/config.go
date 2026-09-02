// Package pipeline is okfpub's command-level glue: it loads the repo-root config
// surface (#156 contract), selects a publishing backend, and drives the three
// stages — Generation, Optimization, Transport — end to end for one publish.
//
// It is the seam cmd/okfpub sits on: main.go parses flags into a LoadOptions and a
// backend choice, and everything below the flag surface lives here so the wiring
// is unit-testable against the in-memory fake backend and the faked Notion HTTP
// surface, offline, without a real workspace.
//
// See sigma/ideas#172 (config contract #156; layout/invocation #165).
package pipeline

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sigma/okf-tools/internal/areas"
	"github.com/sigma/okf-tools/internal/schema"
)

// The two environment variables that carry the Notion credentials in the config
// contract: NOTION_TOKEN (a secret) and NOTION_DB_ID (the data-source id the scan
// queries and top-level page-creates parent under).
const (
	EnvNotionToken = "NOTION_TOKEN"
	EnvNotionDBID  = "NOTION_DB_ID"
	// EnvGDocsImpersonate names the service account the Google Docs backend
	// impersonates. There is deliberately no key-file variable: newer Google
	// organizations forbid service-account keys outright, so the identity is
	// reached from ambient credentials (a developer's gcloud login, or Workload
	// Identity Federation in CI) rather than carried as a secret
	// (sigma/okf-tools#154).
	EnvGDocsImpersonate = "GDOCS_IMPERSONATE_SA"
	// EnvGDriveID names the SHARED DRIVE the document lives in. It must be a shared
	// drive: a service account has no storage quota and cannot own files, so a My
	// Drive folder fails at write time with a misleading 403 (#149).
	EnvGDriveID = "GDRIVE_FOLDER_ID"
)

// Config is the resolved okfpub config surface: the repo-root areas.json and
// schema.json (parsed through the shared loaders) plus the Notion credentials. It
// is the #156 contract made concrete — the committed config that drives the
// mirror's structure, not code. Areas and Schema are nil when their files are
// absent (both are optional: a bundle may be configured entirely through
// okf.toml, and only the Notion backend needs the credentials).
type Config struct {
	// Areas is the parsed /areas.json registry, or nil when none was loaded. It
	// designates the glossary/anchor-host area via a role marker.
	Areas *areas.Registry
	// Schema is the parsed /schema.json column declaration, or nil when none was
	// loaded.
	Schema *schema.Schema
	// NotionToken is the Bearer credential for the Notion backend (NOTION_TOKEN).
	NotionToken string
	// NotionDBID is the Notion data-source id the backend queries and creates under
	// (NOTION_DB_ID).
	NotionDBID string
	// OutDir is the output directory the filesystem/export backend (the dry-run
	// mode) writes its exported tree under. Empty means the backend's own default.
	OutDir string
	// NotionInterval is the minimum spacing between two Notion WRITES, and the rate
	// the read bucket refills at (sigma/okf-tools#134). It is the operator's lever on
	// a run that is pacing-bound — the only remedy for a mirror whose traffic pattern
	// the default does not suit.
	//
	// It is a POINTER because "unset" and "zero" are different answers and the
	// backend's contract gives zero a meaning: nil leaves notion.DefaultInterval in
	// place, while a non-positive value disables pacing outright. Collapsing the two
	// would make `--interval 0` silently pace at the default.
	NotionInterval *time.Duration
	// GDocsImpersonate is the service account the Google Docs backend impersonates
	// (GDOCS_IMPERSONATE_SA).
	GDocsImpersonate string
	// GDriveID is the shared drive the Google Docs backend publishes into
	// (GDRIVE_FOLDER_ID).
	GDriveID string
	// GDocsDryRun, when set, makes the Google Docs backend dump the writes it would
	// perform to this writer instead of issuing them — including the Drive creates
	// in Provision, so --dry-run leaves the destination genuinely untouched rather
	// than only skipping content.
	GDocsDryRun io.Writer
	// GDocsSelection is the area or path being published as one document; empty
	// means the whole bundle. The repeatable flag and the per-area fan-out land in
	// #161 — this carries the single-selection case the backend needs today.
	GDocsSelection string
}

// LoadOptions parameterizes LoadConfig — the resolved flag/env inputs. Paths are
// resolved by the caller (main.go), which defaults them relative to the bundle
// root; an empty path skips that loader, since areas.json and schema.json are both
// optional in the contract.
type LoadOptions struct {
	// AreasPath is the areas.json to load; "" skips it.
	AreasPath string
	// SchemaPath is the schema.json to load; "" skips it.
	SchemaPath string
	// Token, when non-empty, supplies the Notion credential directly instead of
	// reading NOTION_TOKEN — the programmatic seam a caller or test uses to inject a
	// credential. Empty falls back to the environment. The CLI leaves it empty and
	// relies on the environment, since a secret does not belong on a command line.
	Token string
	// DBID is the same override for NOTION_DB_ID; empty falls back to the
	// environment.
	DBID string
	// Getenv is the environment source, injectable for tests; nil uses os.Getenv.
	Getenv func(string) string
}

// LoadConfig resolves the config surface from o. It reads the credentials from the
// explicit args first and the environment second, and loads areas.json / schema.json
// through the shared loaders when their paths are given. A malformed areas.json or
// schema.json is a hard error — the config is authoritative, so it fails loudly
// rather than publishing against a half-understood contract.
func LoadConfig(o LoadOptions) (*Config, error) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	cfg := &Config{
		NotionToken:      firstNonEmpty(o.Token, getenv(EnvNotionToken)),
		NotionDBID:       firstNonEmpty(o.DBID, getenv(EnvNotionDBID)),
		GDocsImpersonate: getenv(EnvGDocsImpersonate),
		GDriveID:         getenv(EnvGDriveID),
	}

	if o.AreasPath != "" {
		reg, err := areas.Load(o.AreasPath)
		if err != nil {
			return nil, fmt.Errorf("load areas: %w", err)
		}
		cfg.Areas = reg
	}
	if o.SchemaPath != "" {
		sc, err := schema.Load(o.SchemaPath)
		if err != nil {
			return nil, fmt.Errorf("load schema: %w", err)
		}
		cfg.Schema = sc
	}
	return cfg, nil
}

// GlossaryFile reports the anchor-host file the areas.json role marker designates,
// for diagnostics and the run summary — "" and false when no registry was loaded or
// none is marked. The publish itself resolves the anchor host through bundle.Load,
// which reads the same marker; this is a read-only convenience over the shared areas
// loader (nil-registry safe), not a second resolution path.
func (c *Config) GlossaryFile() (string, bool) {
	return c.Areas.GlossaryFile()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
