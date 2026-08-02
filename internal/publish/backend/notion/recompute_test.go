package notion

import (
	"context"
	"testing"

	"github.com/sigma/okf-tools/internal/publish/backend"
)

// TestScanRecomputeWalksLiveBlocks: the opt-in recompute reads every node's live
// block tree (the O(nodes) walk), whereas the steady-state ScanStored path reads
// none — the cost boundary that makes recompute opt-in.
func TestScanRecomputeWalksLiveBlocks(t *testing.T) {
	f := newFakeNotion()
	f.rows = []map[string]any{
		row("page-a", map[string]any{"path": richProp("docs/adr/a.md"), "hash": richProp("hStored")}),
	}
	f.liveBlocks["page-a"] = []map[string]any{paraLive("blk-1", "live body")}
	be := newServer(t, f)

	if _, err := be.Scan(context.Background(), backend.ScanStored); err != nil {
		t.Fatalf("ScanStored: %v", err)
	}
	if n := f.countPath("GET", "/blocks/page-a/children"); n != 0 {
		t.Errorf("ScanStored must read no live blocks, got %d block reads", n)
	}

	if _, err := be.Scan(context.Background(), backend.ScanRecompute); err != nil {
		t.Fatalf("ScanRecompute: %v", err)
	}
	if n := f.countPath("GET", "/blocks/page-a/children"); n != 1 {
		t.Errorf("ScanRecompute must walk live blocks, got %d block reads", n)
	}
}

// TestScanRecomputeDetectsLiveDrift: recompute's ContentHash reflects live content,
// so a page whose live blocks differ hashes differently from an untouched one —
// the true-drift signal ScanStored's stored column cannot see.
func TestScanRecomputeDetectsLiveDrift(t *testing.T) {
	f := newFakeNotion()
	f.rows = []map[string]any{
		row("page-a", map[string]any{"path": richProp("a.md"), "hash": richProp("hStored")}),
		row("page-b", map[string]any{"path": richProp("b.md"), "hash": richProp("hStored")}),
	}
	f.liveBlocks["page-a"] = []map[string]any{paraLive("a1", "original body")}
	f.liveBlocks["page-b"] = []map[string]any{paraLive("b1", "HAND EDITED body")}
	be := newServer(t, f)

	// ScanStored reports both nodes with the identical stored hash (blind to drift).
	stored, err := be.Scan(context.Background(), backend.ScanStored)
	if err != nil {
		t.Fatalf("ScanStored: %v", err)
	}
	ha, _ := stored.ContentHash("node:a.md")
	hb, _ := stored.ContentHash("node:b.md")
	if ha != "hStored" || hb != "hStored" {
		t.Errorf("ScanStored should report the stored hash for both, got %q / %q", ha, hb)
	}

	// ScanRecompute hashes live content, so the two differing bodies hash apart.
	rec, err := be.Scan(context.Background(), backend.ScanRecompute)
	if err != nil {
		t.Fatalf("ScanRecompute: %v", err)
	}
	ra, _ := rec.ContentHash("node:a.md")
	rb, _ := rec.ContentHash("node:b.md")
	if ra == rb {
		t.Errorf("recompute should hash differing live content apart, both = %q", ra)
	}
	if ra == "hStored" {
		t.Errorf("recompute hash should be a live fingerprint, not the stored column")
	}
}

// TestScanRecomputeSelfHealsSubpageAndAnchor is the acceptance property: a stored
// subpage id and a stored anchor id that have gone stale are re-derived from the
// live walk, while ScanStored keeps returning the stale values.
func TestScanRecomputeSelfHealsSubpageAndAnchor(t *testing.T) {
	f := newFakeNotion()
	f.rows = []map[string]any{
		row("page-root", map[string]any{
			"path": richProp("index.md"),
			// The stored entry's title ("Sub Page") is deliberately NOT the subpath —
			// a live Notion page carries only its title, so recompute must match by
			// the stored title, not assume title == path.
			"hashes": richProp(mustJSON(t, map[string]subtreeEntry{
				"docs/adr/sub.md": {ID: "stale-sub", Hash: "hStaleSub", Title: "Sub Page"},
			})),
		}),
		row("page-g", map[string]any{
			"path":    richProp("CONTEXT.md"),
			"anchors": richProp(mustJSON(t, map[string]string{"glossary/root-kek": "stale-block"})),
		}),
	}
	// The live tree: the subpage now lives at a fresh id under the root (its stored
	// id went stale); the glossary hosts the anchor's term at a fresh block id.
	f.liveBlocks["page-root"] = []map[string]any{
		paraLive("root-1", "root body"),
		childPageLive("fresh-sub", "Sub Page"),
	}
	f.liveBlocks["fresh-sub"] = []map[string]any{paraLive("sub-1", "sub body")}
	f.liveBlocks["page-g"] = []map[string]any{boldLive("fresh-block", "Root KEK")}
	be := newServer(t, f)

	// ScanStored returns the stale ids straight from the columns.
	stored, err := be.Scan(context.Background(), backend.ScanStored)
	if err != nil {
		t.Fatalf("ScanStored: %v", err)
	}
	if id, _ := stored.NodeID("node:docs/adr/sub.md"); id != "stale-sub" {
		t.Errorf("ScanStored subpage id = %q, want the stale stored id", id)
	}
	if id, _ := stored.AnchorID("glossary/root-kek"); id != "stale-block" {
		t.Errorf("ScanStored anchor id = %q, want the stale stored id", id)
	}

	// ScanRecompute heals both from the live walk.
	rec, err := be.Scan(context.Background(), backend.ScanRecompute)
	if err != nil {
		t.Fatalf("ScanRecompute: %v", err)
	}
	if id, ok := rec.NodeID("node:docs/adr/sub.md"); !ok || id != "fresh-sub" {
		t.Errorf("recompute subpage id = %q,%v, want fresh-sub (self-healed)", id, ok)
	}
	if id, ok := rec.AnchorID("glossary/root-kek"); !ok || id != "fresh-block" {
		t.Errorf("recompute anchor id = %q,%v, want fresh-block (self-healed)", id, ok)
	}
	// The top-level node id still resolves, and the walk reached the subpage.
	if id, _ := rec.NodeID("node:index.md"); id != "page-root" {
		t.Errorf("recompute top-level id = %q, want page-root", id)
	}
	if n := f.countPath("GET", "/blocks/fresh-sub/children"); n != 1 {
		t.Errorf("recompute should recurse into the live subpage, got %d reads", n)
	}
}

// TestScanRecomputeFallsBackToStoredSubpage: a subpage the live walk does not
// surface (no matching child_page) keeps its stored id/hash rather than vanishing.
func TestScanRecomputeFallsBackToStoredSubpage(t *testing.T) {
	f := newFakeNotion()
	f.rows = []map[string]any{
		row("page-root", map[string]any{
			"path": richProp("index.md"),
			"hashes": richProp(mustJSON(t, map[string]subtreeEntry{
				"docs/adr/gone.md": {ID: "kept-id", Hash: "kept-hash", Title: "Gone"},
			})),
		}),
	}
	f.liveBlocks["page-root"] = []map[string]any{paraLive("root-1", "root body")}
	be := newServer(t, f)

	rec, err := be.Scan(context.Background(), backend.ScanRecompute)
	if err != nil {
		t.Fatalf("ScanRecompute: %v", err)
	}
	if id, ok := rec.NodeID("node:docs/adr/gone.md"); !ok || id != "kept-id" {
		t.Errorf("unsurfaced subpage should keep its stored id, got %q,%v", id, ok)
	}
}

// TestScanRecomputeMatchesSubpageByStableID: a subpage whose id is unchanged is
// matched by id (refreshing its live hash) regardless of its title, and an unowned
// child_page is skipped rather than given a fabricated subpath node id.
func TestScanRecomputeMatchesSubpageByStableID(t *testing.T) {
	f := newFakeNotion()
	f.rows = []map[string]any{
		row("page-root", map[string]any{
			"path": richProp("index.md"),
			"hashes": richProp(mustJSON(t, map[string]subtreeEntry{
				"docs/adr/sub.md": {ID: "sub-id", Hash: "hOld", Title: "Sub Page"},
			})),
		}),
	}
	f.liveBlocks["page-root"] = []map[string]any{
		childPageLive("sub-id", "Whatever Title"),   // id matches; title ignored
		childPageLive("stray-id", "Hand-made Page"), // owned by no record → skipped
	}
	f.liveBlocks["sub-id"] = []map[string]any{paraLive("s1", "sub body")}
	be := newServer(t, f)

	rec, err := be.Scan(context.Background(), backend.ScanRecompute)
	if err != nil {
		t.Fatalf("ScanRecompute: %v", err)
	}
	if id, ok := rec.NodeID("node:docs/adr/sub.md"); !ok || id != "sub-id" {
		t.Errorf("stable-id subpage should resolve by id, got %q,%v", id, ok)
	}
	// The stray child_page must not have minted a node keyed off its title.
	if _, ok := rec.NodeID("node:Hand-made Page"); ok {
		t.Errorf("an unowned child_page must not fabricate a node id")
	}
}
