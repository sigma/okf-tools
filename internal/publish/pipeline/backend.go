package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/backend/fake"
	"github.com/sigma/okf-tools/internal/publish/backend/fs"
	"github.com/sigma/okf-tools/internal/publish/backend/gdocs"
	"github.com/sigma/okf-tools/internal/publish/backend/notion"
)

// BackendKind names a selectable publishing backend. Selection is explicit and
// testable (#43 acceptance): real publishes drive Notion; tests drive the
// in-memory fake.
type BackendKind string

const (
	// BackendNotion is the real Notion backend — the production target. It requires
	// both credentials.
	BackendNotion BackendKind = "notion"
	// BackendFake is the fully in-memory fake backend — the primary test harness,
	// needing no credentials and touching no network.
	BackendFake BackendKind = "fake"
	// BackendGDocs is the Google Docs backend: one document per selection, one tab
	// per page, in a shared drive. It needs GDRIVE_FOLDER_ID and, in practice,
	// GDOCS_IMPERSONATE_SA; credentials come from the ambient environment rather
	// than a key file.
	BackendGDocs BackendKind = "gdocs"
	// BackendFS is the filesystem/export backend — okfpub's dry-run / export mode.
	// It writes the bundle as a tree of files under Config.OutDir instead of
	// touching a live workspace, needing no credentials and no network.
	BackendFS BackendKind = "fs"
)

// DefaultOutDir is where the filesystem/export backend writes when --out is not
// given — a visible, gitignore-friendly directory beside the bundle.
const DefaultOutDir = "okfpub-export"

// SelectBackend constructs the backend named by kind from the resolved config. The
// Notion backend requires both NOTION_TOKEN and NOTION_DB_ID and is refused loudly
// without them (rather than deferring to an opaque HTTP 401 mid-publish); the fake
// needs nothing. An unknown kind is an error naming the valid choices.
//
// SelectBackend never contacts the network: notion.New only builds a client aimed
// at the API; the first request happens later, inside Run's Scan/Execute.
func SelectBackend(ctx context.Context, kind BackendKind, cfg *Config, bundleName string) (backend.Backend, error) {
	switch kind {
	case BackendNotion:
		if cfg.NotionToken == "" {
			return nil, fmt.Errorf("backend %q requires %s", BackendNotion, EnvNotionToken)
		}
		if cfg.NotionDBID == "" {
			return nil, fmt.Errorf("backend %q requires %s", BackendNotion, EnvNotionDBID)
		}
		opts := []notion.Option{
			notion.WithToken(cfg.NotionToken),
			notion.WithDataSourceID(cfg.NotionDBID),
			notion.WithSchema(cfg.Schema),
		}
		if d, ok := intervalOverride(cfg); ok {
			opts = append(opts, notion.WithInterval(d))
		}
		return notion.New(opts...), nil
	case BackendGDocs:
		if cfg.GDriveID == "" {
			return nil, fmt.Errorf("backend %q requires %s (a SHARED drive id, not a My Drive folder)",
				BackendGDocs, EnvGDriveID)
		}
		return gdocs.New(ctx, gdocs.Config{
			ImpersonateSA: cfg.GDocsImpersonate,
			DriveID:       cfg.GDriveID,
			Bundle:        bundleName,
			Selection:     cfg.GDocsSelection,
		})
	case BackendFake:
		return fake.New(), nil
	case BackendFS:
		out := cfg.OutDir
		if out == "" {
			out = DefaultOutDir
		}
		return fs.New(fs.WithRoot(out)), nil
	default:
		return nil, fmt.Errorf("unknown backend %q (want %q, %q, %q or %q)",
			kind, BackendNotion, BackendGDocs, BackendFake, BackendFS)
	}
}

// intervalOverride reports the write-pacing interval the operator configured, and
// whether they configured one at all. The two answers must stay separate: unset
// leaves notion.DefaultInterval in place, while an explicit zero disables pacing —
// so collapsing them (treating zero as "unset") would make `--interval 0` silently
// pace at the default, which is the opposite of what it says.
func intervalOverride(cfg *Config) (time.Duration, bool) {
	if cfg == nil || cfg.NotionInterval == nil {
		return 0, false
	}
	return *cfg.NotionInterval, true
}
