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
	"time"

	"golang.org/x/oauth2"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/graph"
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
	// citations remembers where each tab cites an anchor, in ABSOLUTE ranges, so a
	// citation can be re-linked when its target is re-minted by a rewrite (#171).
	// Keyed by tabId, and replaced wholesale on each write of that tab.
	citations map[string][]anchorCitation
	// dryHeadingCount counts the placeholder heading ids a dry run has handed out,
	// which is what makes each one distinct (#171).
	dryHeadingCount int
	// missing records that a dry run found no destination, so reads are skipped and
	// the dump shows what a first publish would do.
	missing bool
	// adoptable are tabs a create retitles instead of adding one beside them: the
	// default tab Docs creates with every new document, and any empty tab with no
	// identity marker, which is what a create whose content write failed leaves
	// behind (#216). Consumed in document order.
	adoptable []strayTab
	// unmarked are the tabs this run created whose identity marker is not on yet,
	// keyed by node: the marker goes on with the content, usually in a LATER
	// transaction, so a failure there is what strands a tab (#216).
	unmarked map[string]createdTab
	// notices are what Provision found the operator must act on: a part created,
	// or a document an earlier split published into and this one no longer does.
	notices []string
}

// Config is the backend's construction input. Endpoints default to the real
// APIs and are overridden only by tests.
type Config struct {
	// ImpersonateSA is the service account to impersonate (GDOCS_IMPERSONATE_SA).
	// Empty means "publish as the ambient credentials", which is valid but not the
	// documented path.
	ImpersonateSA string
	// DriveID is where the document lives (GDRIVE_FOLDER_ID): either a shared drive
	// or a folder INSIDE one (#168). It stays the single user-facing input; the
	// backend resolves it into a search corpus and a write parent at Provision.
	//
	// It must be on a SHARED drive: a service account has no storage quota and
	// cannot own files, so a My Drive folder fails at write time (#149). A folder
	// destination needs only a folder-level grant, not drive membership.
	DriveID string
	// Bundle names the bundle, and Selection the area or path being published.
	// Together they form the identity key stamped into the document's
	// appProperties, so two bundles publishing into one drive cannot collide (#151).
	Bundle    string
	Selection string
	// Part and Parts place this document in an area split across several by size:
	// part Part of Parts, 1-based, and both zero when the area fits one (#216).
	// The part index extends the identity key, so each part is found again by its
	// own; Parts only titles the document at creation.
	Part, Parts int
	// Title is the document's display name at creation. A later human rename is
	// never re-asserted: the document is found by key, so the rename sticks (#151).
	Title string

	DocsEndpoint  string
	DriveEndpoint string
	IAMEndpoint   string
	// DryRunWriter, when set, makes every WRITE dump its payload here instead of
	// issuing it — including the Drive creates in Provision, so a dry run really
	// does leave the destination untouched rather than only skipping content.
	//
	// The interesting failure mode for this backend is "did I build the right
	// batchUpdate", which a rendered Markdown tree cannot show.
	DryRunWriter io.Writer

	// HTTPClient overrides the authenticated transport. Tests pass an unauthenticated
	// client aimed at an httptest server; production leaves it nil.
	HTTPClient *http.Client
	// RequestTimeout bounds ONE attempt's round trip; zero takes
	// DefaultRequestTimeout. A NEGATIVE value disables the bound, which is the
	// unbounded behaviour a stalled stream hangs forever on (#184) and exists only
	// so a test can opt out. notion.WithRequestTimeout reads zero and negative the
	// same way.
	RequestTimeout time.Duration
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
	_ backend.Noticer         = (*Backend)(nil)
	_ graph.AreaRootPublisher = (*Backend)(nil)
)

// PublishesAreaRoots reports true: a document has no rows, so the reason Notion
// excludes an area's landing README does not apply, and an area's document should
// open with the area's own overview (#163).
func (b *Backend) PublishesAreaRoots() bool { return true }

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
		return nil, fmt.Errorf("gdocs: a shared drive or folder id is required")
	}

	timeout := cfg.RequestTimeout
	if timeout == 0 {
		timeout = DefaultRequestTimeout
	}

	c := &client{
		http: cfg.HTTPClient, docs: cfg.DocsEndpoint, drive: cfg.DriveEndpoint,
		dry: cfg.DryRunWriter, timeout: timeout,
	}
	if c.http == nil {
		// A token minted on a retry is traffic too, so it counts as one (#218).
		onRetry := func() { c.count(func(s *publish.RequestStats) { s.Transient++ }) }
		ts, err := tokenSource(ctx, cfg.ImpersonateSA, cfg.IAMEndpoint, onRetry)
		if err != nil {
			return nil, err
		}
		c.http = oauth2.NewClient(ctx, ts)
	}
	return &Backend{
		cfg:       cfg,
		c:         c,
		tabs:      map[string]string{},
		citations: map[string][]anchorCitation{},
		hashes:    map[string]nodeState{},
		titles:    map[string]string{},
		pending:   map[string]*pendingTab{},
		unmarked:  map[string]createdTab{},
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

// Notices reports what the run found that the operator must act on outside it.
// Each part of a split area is one NotebookLM source, so a change in how many
// parts there are is a change to every notebook that loads the area (#216).
func (b *Backend) Notices() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.notices...)
}

// strayTab is a tab no node claims, as the scan found it.
type strayTab struct{ id, title string }

// createdTab is a tab this run created and has not marked yet, and whether it
// was adopted rather than added.
type createdTab struct {
	strayTab
	adopted bool
}

// pendingTab is one node's accumulated body for this run.
type pendingTab struct {
	props  []setProps
	blocks []contentBlock
}

// selectionKey is the identity stamped into appProperties: bundle-qualified so
// two bundles publishing into one shared drive cannot collide (#151).
func (b *Backend) selectionKey() string { return b.keyFor(b.cfg.Part) }

// keyFor is the identity key of one part of this selection; part 0 is the
// unsplit document.
func (b *Backend) keyFor(part int) string {
	sel := b.cfg.Selection
	if sel == "" {
		sel = "."
	}
	key := b.cfg.Bundle + "/" + sel
	if part > 0 {
		key += fmt.Sprintf("#%d", part)
	}
	return key
}

// docTitle is the document's name at creation.
//
// A part's "(i/N)" is set only here, like the rest of the title: a later run
// that splits the area differently does not retitle a document it finds.
func (b *Backend) docTitle() string {
	title := b.cfg.Title
	switch {
	case title != "":
	case b.cfg.Selection != "":
		title = fmt.Sprintf("%s — %s", b.cfg.Bundle, b.cfg.Selection)
	default:
		title = b.cfg.Bundle
	}
	if b.cfg.Parts > 0 {
		title += fmt.Sprintf(" (%d/%d)", b.cfg.Part, b.cfg.Parts)
	}
	return title
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
