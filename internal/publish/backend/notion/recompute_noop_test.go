package notion

import (
	"context"
	"fmt"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/graph"
)

// liveBlockMaps renders a doc through the Executor's projection into the raw Notion
// block JSON a GET /blocks/{id}/children would serve after publishing it — the live
// state the recompute walk reconstructs from. plain_text is stamped as the server
// fills it, and a page mention keeps a title plain_text the scanner must discard.
// (The noop bundle carries no tables, so no table_row children need seeding.)
func liveBlockMaps(t *testing.T, be *Backend, d *bundle.Doc, pageID string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for i, u := range be.Tokenize(graph.ProjectDocument(d, nil)) {
		cb, ok := u.Payload.(childBlock)
		if !ok {
			t.Fatalf("unit %d payload is %T, want childBlock", i, u.Payload)
		}
		if cb.kind == int(graph.Table) {
			t.Fatalf("liveBlockMaps does not seed table children; noop bundle should have none")
		}
		typ, payload, err := blockContentJSON(cb, agreeResolver{})
		if err != nil {
			t.Fatalf("blockContentJSON: %v", err)
		}
		if runs, ok := payload["rich_text"].([]map[string]any); ok {
			annotateRuns(runs)
		}
		out = append(out, map[string]any{
			"object": "block", "id": fmt.Sprintf("%s-blk%d", pageID, i), "type": typ, typ: payload,
		})
	}
	return out
}

// TestScanRecomputeUnchangedCorpusHashSkips is the keystone of #110: after the whole
// fix, an unchanged corpus produces ZERO ops under --recompute. It seeds the fake so
// every page's live blocks reconstruct to exactly the source content hash and every
// row carries the matching (stored-readback) property hash, then runs the real
// ScanRecompute → Generate(with the wired source hasher) and asserts the op-DAG is
// empty — the drift-heal steady-state property the ticket is about, which before the
// alignment re-clobbered every page every run.
func TestScanRecomputeUnchangedCorpusHashSkips(t *testing.T) {
	b := loadNoopBundle(t)
	f := newFakeNotion()
	be := newServer(t, f)

	for _, d := range b.Docs {
		pageID := "page-" + d.Rel
		f.liveBlocks[pageID] = liveBlockMaps(t, be, d, pageID)
		// The `hash` column's property half is read back under recompute (content is
		// reconstructed live); pair it with an arbitrary content half.
		f.rows = append(f.rows, row(pageID, map[string]any{
			"path": richProp(d.Rel),
			"hash": richProp(encodeHashPair(be.sourceContentHash(d, nil), graph.PropertyHash(d))),
		}))
	}

	ctx := context.Background()
	scan, err := be.Scan(ctx, backend.ScanRecompute)
	if err != nil {
		t.Fatalf("ScanRecompute: %v", err)
	}
	g, err := graph.Generate(ctx, b, scan, graph.WithHasher(be.RecomputeContentHasher(nil)))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(g.Ops) != 0 {
		var kinds []string
		for _, op := range g.Ops {
			kinds = append(kinds, fmt.Sprintf("%s(%s)", op.Kind, op.Node))
		}
		t.Fatalf("unchanged corpus under --recompute should emit no ops, got %d: %v", len(g.Ops), kinds)
	}
}

// TestScanRecomputeBodyDriftRepublishes is the guard rail: a genuinely hand-edited
// page still re-publishes under --recompute (only that page), so the alignment did
// not blind drift detection.
func TestScanRecomputeBodyDriftRepublishes(t *testing.T) {
	b := loadNoopBundle(t)
	f := newFakeNotion()
	be := newServer(t, f)

	drifted := "docs/adr/b.md"
	for _, d := range b.Docs {
		pageID := "page-" + d.Rel
		blocks := liveBlockMaps(t, be, d, pageID)
		if d.Rel == drifted {
			// A hand edit in Notion: append a stray live paragraph the source lacks.
			blocks = append(blocks, paraLive(pageID+"-drift", "edited directly in Notion"))
		}
		f.liveBlocks[pageID] = blocks
		f.rows = append(f.rows, row(pageID, map[string]any{
			"path": richProp(d.Rel),
			"hash": richProp(encodeHashPair(be.sourceContentHash(d, nil), graph.PropertyHash(d))),
		}))
	}

	ctx := context.Background()
	scan, err := be.Scan(ctx, backend.ScanRecompute)
	if err != nil {
		t.Fatalf("ScanRecompute: %v", err)
	}
	g, err := graph.Generate(ctx, b, scan, graph.WithHasher(be.RecomputeContentHasher(nil)))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(g.Ops) == 0 {
		t.Fatal("a live body edit under --recompute must re-publish its page, got no ops")
	}
	for _, op := range g.Ops {
		if op.Node != publish.SymbolicID("node:"+drifted) {
			t.Errorf("only the drifted page should re-publish, got op on %s", op.Node)
		}
	}
}
