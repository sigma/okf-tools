package pipeline

import (
	"fmt"

	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/backend/fake"
	"github.com/sigma/okf-tools/internal/publish/backend/fs"
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
func SelectBackend(kind BackendKind, cfg *Config) (backend.Backend, error) {
	switch kind {
	case BackendNotion:
		if cfg.NotionToken == "" {
			return nil, fmt.Errorf("backend %q requires %s", BackendNotion, EnvNotionToken)
		}
		if cfg.NotionDBID == "" {
			return nil, fmt.Errorf("backend %q requires %s", BackendNotion, EnvNotionDBID)
		}
		return notion.New(
			notion.WithToken(cfg.NotionToken),
			notion.WithDataSourceID(cfg.NotionDBID),
		), nil
	case BackendFake:
		return fake.New(), nil
	case BackendFS:
		out := cfg.OutDir
		if out == "" {
			out = DefaultOutDir
		}
		return fs.New(fs.WithRoot(out)), nil
	default:
		return nil, fmt.Errorf("unknown backend %q (want %q, %q or %q)", kind, BackendNotion, BackendFake, BackendFS)
	}
}
