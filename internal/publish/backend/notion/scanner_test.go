package notion

import (
	"context"
	"slices"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
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

// TestScanStoredSkipsStrayRows: a row with no path is a hand-made row the pipeline
// never created and is skipped.
func TestScanStoredSkipsStrayRows(t *testing.T) {
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
	count := 0
	for range cs.Nodes() {
		count++
	}
	if count != 1 {
		t.Errorf("stray row should be skipped, Nodes count = %d, want 1", count)
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
