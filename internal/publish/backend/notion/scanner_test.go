package notion

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/optimize"
	"github.com/sigma/okf-tools/internal/publish/transport"
)

// TestScanStoredSeedsCurrentState: one data-source query over self-describing
// derived columns seeds every consumer query — top-level ids/hashes, subtree-map
// subpage ids/hashes, and the glossary anchor map — with no per-page block reads.
func TestScanStoredSeedsCurrentState(t *testing.T) {
	f := newFakeNotion()
	f.rows = []map[string]any{
		row("page-a", map[string]any{
			"path": richProp("docs/adr/a.md"),
			"hash": richProp("hA"),
		}),
		row("page-g", map[string]any{
			"path":    richProp("CONTEXT.md"),
			"hash":    richProp("hG"),
			"anchors": richProp(mustJSON(t, map[string]string{"glossary/root-kek": "block-kek"})),
		}),
		row("page-root", map[string]any{
			"path": richProp("index.md"),
			"hashes": richProp(mustJSON(t, map[string]subtreeEntry{
				"docs/adr/sub.md": {ID: "page-sub", Hash: "hS"},
			})),
		}),
	}
	be := newServer(t, f)

	cs, err := be.Scan(context.Background(), backend.ScanStored)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// Top-level node id + hash.
	if id, ok := cs.NodeID("node:docs/adr/a.md"); !ok || id != "page-a" {
		t.Errorf("NodeID(a.md) = %q,%v, want page-a", id, ok)
	}
	if h, ok := cs.ContentHash("node:docs/adr/a.md"); !ok || h != "hA" {
		t.Errorf("ContentHash(a.md) = %q,%v, want hA", h, ok)
	}
	// Subtree-map subpage id + hash (the #146 {id,hash} upgrade).
	if id, ok := cs.NodeID("node:docs/adr/sub.md"); !ok || id != "page-sub" {
		t.Errorf("NodeID(sub.md) = %q,%v, want page-sub from the subtree map", id, ok)
	}
	if h, ok := cs.ContentHash("node:docs/adr/sub.md"); !ok || h != "hS" {
		t.Errorf("ContentHash(sub.md) = %q,%v, want hS", h, ok)
	}
	// Glossary anchor id.
	if id, ok := cs.AnchorID("glossary/root-kek"); !ok || id != "block-kek" {
		t.Errorf("AnchorID(root-kek) = %q,%v, want block-kek", id, ok)
	}
	// Nodes() enumerates top-level rows and subtree members.
	var nodes []publish.SymbolicID
	for n := range cs.Nodes() {
		nodes = append(nodes, n)
	}
	for _, want := range []publish.SymbolicID{
		"node:docs/adr/a.md", "node:CONTEXT.md", "node:index.md", "node:docs/adr/sub.md",
	} {
		if !slices.Contains(nodes, want) {
			t.Errorf("Nodes() = %v, missing %s", nodes, want)
		}
	}
}

// TestScanStoredPaginates: a data source larger than one page drains every page via
// the cursor.
func TestScanStoredPaginates(t *testing.T) {
	f := newFakeNotion()
	f.pageSize = 1
	f.rows = []map[string]any{
		row("p1", map[string]any{"path": richProp("a.md")}),
		row("p2", map[string]any{"path": richProp("b.md")}),
		row("p3", map[string]any{"path": richProp("c.md")}),
	}
	be := newServer(t, f)

	cs, err := be.Scan(context.Background(), backend.ScanStored)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if n := f.countPath("POST", "/data_sources/ds1/query"); n != 3 {
		t.Errorf("want 3 paginated queries, got %d", n)
	}
	for _, want := range []publish.SymbolicID{"node:a.md", "node:b.md", "node:c.md"} {
		if _, ok := cs.NodeID(want); !ok {
			t.Errorf("row %s missing after pagination", want)
		}
	}
}

// TestScanStoredSurfacesPathlessRowsAsUnclaimed: a row with no path names no
// source node — most often one an aborted run created and never recorded (#135).
// The scan surfaces it as an unclaimed object keyed by its row id, so
// reconciliation can reclaim it, instead of dropping it on the floor where it
// leaks one row per aborted run forever.
func TestScanStoredSurfacesPathlessRowsAsUnclaimed(t *testing.T) {
	f := newFakeNotion()
	f.rows = []map[string]any{
		row("owned", map[string]any{"path": richProp("a.md")}),
		row("stray", map[string]any{}), // no path column
	}
	be := newServer(t, f)

	cs, err := be.Scan(context.Background(), backend.ScanStored)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if id, ok := cs.NodeID(publish.NodeRef("a.md")); !ok || id != "owned" {
		t.Errorf("owned row = %q,%v, want the owned page id", id, ok)
	}
	unclaimed := publish.UnclaimedRef("stray")
	id, ok := cs.NodeID(unclaimed)
	if !ok || id != "stray" {
		t.Errorf("path-less row = %q,%v, want it surfaced as unclaimed resolving to its row id", id, ok)
	}
	// It is unclaimed, not a node: nothing may read a path off it.
	for scanned := range cs.Nodes() {
		if _, isUnclaimed := scanned.Unclaimed(); isUnclaimed {
			continue
		}
		if scanned != publish.NodeRef("a.md") {
			t.Errorf("unexpected node in the snapshot: %s", scanned)
		}
	}
}

// TestScanStoredDuplicatePathErrors: two pages claiming one path is unrepairable
// state and a hard error, never silent last-writer-wins.
func TestScanStoredDuplicatePathErrors(t *testing.T) {
	f := newFakeNotion()
	f.rows = []map[string]any{
		row("p1", map[string]any{"path": richProp("dup.md")}),
		row("p2", map[string]any{"path": richProp("dup.md")}),
	}
	be := newServer(t, f)

	if _, err := be.Scan(context.Background(), backend.ScanStored); err == nil {
		t.Fatal("expected a hard error for a duplicated path")
	}
}

// TestScanUnchangedGlossaryResolvesAnchorsFromSeed: the acceptance property — an
// unchanged glossary's anchors resolve straight from the scan seed, with no
// rewrite. Once seeded into a resolution table, a page's anchor Ref resolves
// without the glossary being touched.
func TestScanUnchangedGlossaryResolvesAnchorsFromSeed(t *testing.T) {
	f := newFakeNotion()
	f.rows = []map[string]any{
		row("page-g", map[string]any{
			"path":    richProp("CONTEXT.md"),
			"anchors": richProp(mustJSON(t, map[string]string{"glossary/root-kek": "block-kek"})),
		}),
	}
	be := newServer(t, f)

	cs, err := be.Scan(context.Background(), backend.ScanStored)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	// The anchor resolves from the seed alone — no SetContent, no rewrite of the
	// glossary was needed to make a linking page's anchor Ref resolvable.
	if id, ok := cs.AnchorID("glossary/root-kek"); !ok || id != "block-kek" {
		t.Errorf("unchanged glossary anchor should seed directly, got %q,%v", id, ok)
	}
}

// TestScanQueryPinsDataSourceAPIVersion: the scan queries the 2025-09-03-only
// route POST /data_sources/{id}/query, so the default Notion-Version it pins must
// name that API surface. Under an older version (e.g. 2022-06-28) that route does
// not exist and Notion answers 400 invalid_request_url before anything publishes
// (sigma/okf-tools#78).
func TestScanQueryPinsDataSourceAPIVersion(t *testing.T) {
	f := newFakeNotion()
	f.rows = []map[string]any{
		row("page-a", map[string]any{
			"path": richProp("docs/adr/a.md"),
			"hash": richProp("hA"),
		}),
	}
	be := newServer(t, f)

	if _, err := be.Scan(context.Background(), backend.ScanStored); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	reqs := f.requestsTo("POST", "/data_sources/ds1/query")
	if len(reqs) == 0 {
		t.Fatalf("scan issued no data-source query")
	}
	for _, r := range reqs {
		if r.Version != "2025-09-03" {
			t.Errorf("scan query Notion-Version = %q, want 2025-09-03 (the data-source API surface)", r.Version)
		}
	}
}

// A publish reclaims the rows an aborted run left behind: an unclaimed row is
// archived exactly as a vanished node is, end to end through Generation,
// Optimization and the transport (#135). Running it twice archives nothing further.
func TestPublishArchivesUnclaimedRowsAndIsIdempotent(t *testing.T) {
	b := loadNoopBundle(t)
	f := newFakeNotion()
	be := newServer(t, f)
	ctx := context.Background()

	// Steady state for every source page, plus rows the mirror should reclaim: two
	// an earlier run created and died before recording (no path), and one that has a
	// path but no source left — the ordinary vanished node, which must keep behaving
	// exactly as it did.
	for _, d := range b.Docs {
		f.rows = append(f.rows, row("page-"+d.Rel, map[string]any{
			"path": richProp(d.Rel),
			"hash": richProp(encodeHashPair(be.sourceContentHash(d, nil), graph.PropertyHash(d))),
		}))
	}
	f.rows = append(f.rows,
		row("leaked-1", map[string]any{}),
		row("leaked-2", map[string]any{"hash": richProp("h")}), // hash but no path
		row("vanished", map[string]any{"path": richProp("gone.md"), "hash": richProp("h")}),
	)

	publish1 := publishOnce(t, ctx, be, b)
	if publish1.reclaimed != 2 {
		t.Errorf("first run reclaimed %d unclaimed row(s), want 2", publish1.reclaimed)
	}

	for _, id := range []string{"leaked-1", "leaked-2", "vanished"} {
		reqs := f.requestsTo(http.MethodPatch, "/pages/"+id)
		if len(reqs) != 1 {
			t.Errorf("row %s: %d archive request(s), want exactly 1", id, len(reqs))
			continue
		}
		if archived, _ := reqs[0].Body["archived"].(bool); !archived {
			t.Errorf("row %s: PATCH body = %v, want archived:true", id, reqs[0].Body)
		}
	}
	// Reclaiming a leak must not drag the rest of the mirror into a rewrite.
	if got := f.countPath(http.MethodPost, "/pages"); got != 0 {
		t.Errorf("reclaim run created %d page(s), want 0", got)
	}

	// Second run over the state the first one left: the archived rows are gone from
	// the query, so there is nothing left to reclaim and nothing at all to do.
	publish2 := publishOnce(t, ctx, be, b)
	if publish2.ops != 0 {
		t.Errorf("second run planned %d op(s), want none: %v", publish2.ops, publish2.opKinds)
	}
	if publish2.reclaimed != 0 {
		t.Errorf("second run reclaimed %d row(s), want 0", publish2.reclaimed)
	}
}

// publishRun is what one drive of the pipeline stages reports back to a test.
type publishRun struct {
	ops       int
	reclaimed int
	opKinds   []string
}

// publishOnce drives Scan → Generate → Optimize → Transport against the live fake,
// the same wiring pipeline.Run uses, and reports what the run planned.
func publishOnce(t *testing.T, ctx context.Context, be *Backend, b *bundle.Bundle) publishRun {
	t.Helper()
	scan, err := be.Scan(ctx, backend.ScanStored)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	g, err := graph.Generate(ctx, b, scan, graph.WithHasher(be.RecomputeContentHasher(nil)))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := transport.New(be).Run(ctx, optimize.Optimize(g, be, be), scan); err != nil {
		t.Fatalf("transport: %v", err)
	}
	out := publishRun{ops: len(g.Ops), reclaimed: g.UnclaimedDeletes()}
	for _, op := range g.Ops {
		out.opKinds = append(out.opKinds, op.Kind.String()+" "+string(op.Node))
	}
	return out
}
