// Package gdocs publishes an OKF bundle selection to a Google Doc: one tab per
// page, in one document, found-or-created inside a configured shared drive.
//
// It is the third backend behind the publishing seam, after Notion and the
// filesystem export, and the first whose destination is a DOCUMENT rather than a
// database of pages. Two carve-outs were accepted for that shape
// (sigma/okf-tools#152), and they live entirely inside this package:
//
//   - SetProperties has no home. A tab has three writable properties (title,
//     parentTabId, iconEmoji) and none of them holds frontmatter, so properties
//     are rendered as content rather than asserted separately.
//   - Anchors need read-after-write. ParagraphStyle.headingId is documented
//     read-only and the batchUpdate reply does not return it, so an anchor hosted
//     and cited by one transaction needs write → read → write.
//
// Everything else is ordinary: the pipeline drives this backend unchanged.
//
// Tabs are FLAT (#155): the parent edge Generation computes is consumed for
// ordering but never written to parentTabId, and the hierarchy stays visible
// because an index page's links resolve to intra-document tab links.
package gdocs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/oauth2"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// Backend implements every publishing role against Google Docs and Drive.
type Backend struct {
	cfg Config
	c   *client

	// mu guards the mutable run state below: Provision fills the ids, Scan seeds
	// the tab map, and Execute extends it.
	mu sync.Mutex
	// docID is the destination document, resolved by Provision.
	docID string
	// stateID is the sidecar state file beside it.
	stateID string
	// tabs maps a node's bundle-relative path to its live tabId.
	tabs map[string]string
	// hashes is the sidecar's stored per-node state, seeded by Scan.
	hashes map[string]nodeState
	// titles maps a claimed tab title to the node that claimed it, so a collision
	// can be disambiguated rather than producing two identically named tabs.
	titles map[string]string
	// pending accumulates a node's rendered parts ACROSS the transactions of one
	// run. The optimizer splits a node's create, properties and content into
	// separate transactions, and every write here replaces a tab's whole body — so
	// without accumulation the content write would wipe the properties written a
	// moment earlier. Keyed by bundle-relative path.
	pending map[string]*pendingTab
	// adoptable is the document's default tab, which Docs creates with every new
	// document. The first published page adopts it instead of adding a tab beside
	// it; empty once claimed or once the document has real content.
	adoptable string
}

// Config is the backend's construction input. Endpoints default to the real
// APIs and are overridden only by tests.
type Config struct {
	// ImpersonateSA is the service account to impersonate (GDOCS_IMPERSONATE_SA).
	// Empty means "publish as the ambient credentials", which is valid but not the
	// documented path.
	ImpersonateSA string
	// DriveID is the shared drive the document lives in (GDRIVE_FOLDER_ID). It must
	// be a SHARED drive: a service account has no storage quota and cannot own
	// files, so a My Drive folder fails at write time (#149).
	DriveID string
	// Bundle names the bundle, and Selection the area or path being published.
	// Together they form the identity key stamped into the document's
	// appProperties, so two bundles publishing into one drive cannot collide (#151).
	Bundle    string
	Selection string
	// Title is the document's display name at creation. A later human rename is
	// never re-asserted: the document is found by key, so the rename sticks (#151).
	Title string

	DocsEndpoint  string
	DriveEndpoint string
	IAMEndpoint   string
	// DryRunWriter, when set, makes Execute DUMP the batchUpdate requests it would
	// issue instead of issuing them. The interesting failure mode here is "did I
	// build the right batchUpdate", which the filesystem export cannot show.
	DryRunWriter io.Writer

	// HTTPClient overrides the authenticated transport. Tests pass an unauthenticated
	// client aimed at an httptest server; production leaves it nil.
	HTTPClient *http.Client
}

var (
	_ backend.Tokenizer       = (*Backend)(nil)
	_ backend.ConstraintModel = (*Backend)(nil)
	_ backend.Executor        = (*Backend)(nil)
	_ backend.Scanner         = (*Backend)(nil)
	_ backend.WriteBacker     = (*Backend)(nil)
	_ backend.Backend         = (*Backend)(nil)
	_ backend.Provisioner     = (*Backend)(nil)
	_ backend.RequestReporter = (*Backend)(nil)
)

// New builds a backend from cfg, wiring credentials unless a transport is
// supplied. It performs no I/O: the first request happens inside Provision.
func New(ctx context.Context, cfg Config) (*Backend, error) {
	if cfg.DocsEndpoint == "" {
		cfg.DocsEndpoint = DefaultDocsEndpoint
	}
	if cfg.DriveEndpoint == "" {
		cfg.DriveEndpoint = DefaultDriveEndpoint
	}
	if cfg.IAMEndpoint == "" {
		cfg.IAMEndpoint = DefaultIAMEndpoint
	}
	if cfg.DriveID == "" {
		return nil, fmt.Errorf("gdocs: a shared drive id is required")
	}

	hc := cfg.HTTPClient
	if hc == nil {
		ts, err := tokenSource(ctx, cfg.ImpersonateSA, cfg.IAMEndpoint)
		if err != nil {
			return nil, err
		}
		hc = oauth2.NewClient(ctx, ts)
	}
	return &Backend{
		cfg:     cfg,
		c:       &client{http: hc, docs: cfg.DocsEndpoint, drive: cfg.DriveEndpoint},
		tabs:    map[string]string{},
		hashes:  map[string]nodeState{},
		titles:  map[string]string{},
		pending: map[string]*pendingTab{},
	}, nil
}

// RequestStats reports what the run cost in API traffic.
//
// It is a REQUIREMENT of this backend rather than a nicety: the binding quota is
// 60 writes per minute per user, and this number is the agreed trigger for
// revisiting the per-node transaction grouping (#156).
func (b *Backend) RequestStats() publish.RequestStats { return b.c.RequestStats() }

// DocumentID reports the destination document, for the run summary. Empty before
// Provision.
func (b *Backend) DocumentID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.docID
}

// pendingTab is one node's accumulated body for this run.
type pendingTab struct {
	props  []setProps
	blocks []contentBlock
}

// selectionKey is the identity stamped into appProperties: bundle-qualified so
// two bundles publishing into one shared drive cannot collide (#151).
func (b *Backend) selectionKey() string {
	sel := b.cfg.Selection
	if sel == "" {
		sel = "."
	}
	return b.cfg.Bundle + "/" + sel
}

// docTitle is the document's name at creation.
func (b *Backend) docTitle() string {
	if b.cfg.Title != "" {
		return b.cfg.Title
	}
	if b.cfg.Selection != "" {
		return fmt.Sprintf("%s — %s", b.cfg.Bundle, b.cfg.Selection)
	}
	return b.cfg.Bundle
}

// relOf recovers a node's bundle-relative path from a transaction's group.
func relOf(g publish.GroupKey) (string, error) {
	id := publish.SymbolicID(g)
	if b, ok := id.Unclaimed(); ok {
		return "", fmt.Errorf("gdocs: unclaimed object %s has no path; this backend's scan mints none", b)
	}
	return id.Rel(), nil
}

// stateFileName is the sidecar's display name, derived from the selection so a
// human browsing the drive can tell which document it belongs to.
func (b *Backend) stateFileName() string {
	sel := strings.ReplaceAll(b.selectionKey(), "/", "-")
	return "okf-state-" + sel + ".json"
}
