package notion

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/optimize"
	"github.com/sigma/okf-tools/internal/publish/transport"
)

// TestWriteBackTopLevelRow: a top-level node's provenance is PATCHed onto its own
// row as the `path` and `hash` derived columns (and `anchors` for a glossary row),
// so the next ScanStored reads current values.
func TestWriteBackTopLevelRow(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	prov := publish.Provenance{Nodes: map[publish.SymbolicID]publish.NodeProvenance{
		"node:CONTEXT.md": {
			ID:        "page-g",
			NodeStamp: publish.NodeStamp{Hash: "hG", PropHash: "pG"},
			Anchors:   map[publish.AnchorName]publish.BackendID{"glossary/root-kek": "block-kek"},
		},
	}}
	if err := be.WriteBack(context.Background(), prov); err != nil {
		t.Fatalf("WriteBack: %v", err)
	}

	patches := f.requestsTo("PATCH", "/pages/page-g")
	if len(patches) != 1 {
		t.Fatalf("want 1 property PATCH on the row, got %d", len(patches))
	}
	props := digInto(t, patches[0].Body, "properties")
	if got := columnText(t, props, "path"); got != "CONTEXT.md" {
		t.Errorf("path column = %q, want CONTEXT.md", got)
	}
	// The `hash` column carries the compound content.prop value (two-hash split).
	if got := columnText(t, props, "hash"); got != "hG.pG" {
		t.Errorf("hash column = %q, want hG.pG", got)
	}
	var anchors map[string]string
	if err := json.Unmarshal([]byte(columnText(t, props, "anchors")), &anchors); err != nil {
		t.Fatalf("anchors column not valid JSON: %v", err)
	}
	if anchors["glossary/root-kek"] != "block-kek" {
		t.Errorf("anchors column = %v, want the hosted anchor's block id", anchors)
	}
}

// TestWriteBackSubpageFoldsIntoParentSubtree: a subpage's {id, hash} is merged into
// its parent row's `hashes` subtree map via a read-modify-write, preserving the
// map's other members.
func TestWriteBackSubpageFoldsIntoParentSubtree(t *testing.T) {
	f := newFakeNotion()
	// The parent row already carries one other subpage in its subtree map — the
	// read-modify-write must keep it.
	f.pageProps["page-root"] = map[string]any{
		"hashes": richProp(mustJSON(t, map[string]subtreeEntry{
			"docs/adr/existing.md": {ID: "page-existing", Hash: "hExisting"},
		})),
	}
	be := newServer(t, f)

	prov := publish.Provenance{Nodes: map[publish.SymbolicID]publish.NodeProvenance{
		"node:docs/adr/new.md": {
			ID:        "page-new",
			OwnerID:   "page-root", // one level: the parent IS the owning row
			NodeStamp: publish.NodeStamp{Hash: "hNew", Parent: "node:index.md", Owner: "node:index.md", Title: "New Page"},
		},
	}}
	if err := be.WriteBack(context.Background(), prov); err != nil {
		t.Fatalf("WriteBack: %v", err)
	}

	// It reads the parent's current map, then PATCHes the merged one back.
	if n := f.countPath("GET", "/pages/page-root"); n != 1 {
		t.Errorf("subtree write-back should read the parent row once, got %d", n)
	}
	patches := f.requestsTo("PATCH", "/pages/page-root")
	if len(patches) != 1 {
		t.Fatalf("want 1 subtree PATCH on the parent, got %d", len(patches))
	}
	var merged map[string]subtreeEntry
	if err := json.Unmarshal([]byte(columnText(t, digInto(t, patches[0].Body, "properties"), "hashes")), &merged); err != nil {
		t.Fatalf("hashes column not valid JSON: %v", err)
	}
	if e := merged["docs/adr/new.md"]; e.ID != "page-new" || e.Hash != "hNew" || e.Title != "New Page" {
		t.Errorf("new subpage entry = %+v, want {page-new hNew New Page}", e)
	}
	if e := merged["docs/adr/existing.md"]; e.ID != "page-existing" {
		t.Errorf("read-modify-write dropped the existing subpage entry: %+v", merged)
	}
}

// TestWriteBackChunksOversizedColumn: a derived-column value longer than the per-
// span char cap is split across several rich_text spans, each within the cap, that
// concatenate back to the original — the same cap the block path applies. Notion
// 400s a single span over 2000 chars, and the glossary host's anchors map is the
// first derived column to hit it. See okf-tools#94.
func TestWriteBackChunksOversizedColumn(t *testing.T) {
	f := newFakeNotion()
	// Dial the per-span cap low so a modest value forces splitting, exactly as the
	// block path's WithMaxBlockChars does — no need for a real 2000+ char payload.
	const limit = 5
	be := newServer(t, f, WithMaxBlockChars(limit))

	prov := publish.Provenance{Nodes: map[publish.SymbolicID]publish.NodeProvenance{
		"node:CONTEXT.md": {
			ID:        "page-g",
			NodeStamp: publish.NodeStamp{Hash: "hG", PropHash: "pG"},
			Anchors:   map[publish.AnchorName]publish.BackendID{"glossary/root-kek": "block-kek"},
		},
	}}
	if err := be.WriteBack(context.Background(), prov); err != nil {
		t.Fatalf("WriteBack: %v", err)
	}

	patches := f.requestsTo("PATCH", "/pages/page-g")
	if len(patches) != 1 {
		t.Fatalf("want 1 property PATCH on the row, got %d", len(patches))
	}
	props := digInto(t, patches[0].Body, "properties")

	// The anchors JSON is well over the cap, so it must arrive as several spans,
	// each within the cap — never a single oversized span.
	spans, ok := digInto(t, props, "anchors")["rich_text"].([]any)
	if !ok || len(spans) < 2 {
		t.Fatalf("oversized anchors column should split into >1 span, got %v", props["anchors"])
	}
	for i, s := range spans {
		content := digInto(t, s.(map[string]any), "text")["content"].(string)
		if n := len([]rune(content)); n > limit {
			t.Errorf("span %d holds %d chars, over the cap of %d", i, n, limit)
		}
	}

	// And the spans concatenate back to the exact anchors JSON — round-trip safe.
	var anchors map[string]string
	if err := json.Unmarshal([]byte(columnText(t, props, "anchors")), &anchors); err != nil {
		t.Fatalf("reassembled anchors not valid JSON: %v", err)
	}
	if anchors["glossary/root-kek"] != "block-kek" {
		t.Errorf("reassembled anchors = %v, want the hosted anchor's block id", anchors)
	}
}

// TestWriteBackEmptyIsNoOp: empty provenance (a steady-state unchanged run) writes
// nothing — the near-noop property.
func TestWriteBackEmptyIsNoOp(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)
	if err := be.WriteBack(context.Background(), publish.Provenance{}); err != nil {
		t.Fatalf("WriteBack: %v", err)
	}
	if len(f.reqs) != 0 {
		t.Errorf("empty write-back should issue no requests, got %d", len(f.reqs))
	}
}

// columnText extracts a derived column's value from a PATCH properties body,
// concatenating every rich-text span's content — mirroring plainText, so a value
// chunked across several spans round-trips to its original string.
func columnText(t *testing.T, props map[string]any, key string) string {
	t.Helper()
	col, ok := props[key].(map[string]any)
	if !ok {
		t.Fatalf("column %q missing or not an object: %v", key, props[key])
	}
	spans, ok := col["rich_text"].([]any)
	if !ok || len(spans) == 0 {
		t.Fatalf("column %q has no rich_text spans: %v", key, col)
	}
	var out string
	for _, s := range spans {
		span, _ := s.(map[string]any)
		out += digInto(t, span, "text")["content"].(string)
	}
	return out
}

// --- incremental write-back (#135) ------------------------------------------

// Two subpages of one parent, written back in SEPARATE calls (what per-group
// write-back produces), must both survive in the parent's subtree map. This is the
// read-modify-write's real exposure: the second merge must start from what the
// first one wrote, not from a stale read.
func TestSubtreeMergeAcrossSeparateWriteBacks(t *testing.T) {
	f := newFakeNotion()
	f.pageProps = map[string]map[string]any{
		"page-parent": {"hashes": richProp(mustJSON(t, map[string]subtreeEntry{
			"cluster/old.md": {ID: "page-old", Hash: "hOld"},
		}))},
	}
	be := newServer(t, f)
	ctx := context.Background()

	first := publish.Provenance{Nodes: map[publish.SymbolicID]publish.NodeProvenance{
		"node:cluster/a.md": {
			ID: "page-a", OwnerID: "page-parent",
			NodeStamp: publish.NodeStamp{Hash: "hA", Parent: "node:cluster/index.md", Title: "A"},
		},
	}}
	second := publish.Provenance{Nodes: map[publish.SymbolicID]publish.NodeProvenance{
		"node:cluster/b.md": {
			ID: "page-b", OwnerID: "page-parent",
			NodeStamp: publish.NodeStamp{Hash: "hB", Parent: "node:cluster/index.md", Title: "B"},
		},
	}}
	if err := be.WriteBack(ctx, first); err != nil {
		t.Fatalf("first write-back: %v", err)
	}
	if err := be.WriteBack(ctx, second); err != nil {
		t.Fatalf("second write-back: %v", err)
	}

	// Read the map back the way the next run's scan would.
	patches := f.requestsTo(http.MethodPatch, "/pages/page-parent")
	if len(patches) != 2 {
		t.Fatalf("want one merge per write-back (2), got %d", len(patches))
	}
	f.mu.Lock()
	stored := f.pageProps["page-parent"]["hashes"]
	f.mu.Unlock()

	// Decode it exactly as a scan would: through the server's own property shape.
	raw, err := json.Marshal(map[string]any{"hashes": stored})
	if err != nil {
		t.Fatalf("re-encode stored props: %v", err)
	}
	var served struct {
		Hashes property `json:"hashes"`
	}
	if err := json.Unmarshal(raw, &served); err != nil {
		t.Fatalf("decode stored props: %v", err)
	}
	merged, err := storedSubtree(plainText(served.Hashes), "page-parent")
	if err != nil {
		t.Fatalf("decode merged map: %v", err)
	}
	for subpath, wantHash := range map[string]string{
		"cluster/old.md": "hOld", "cluster/a.md": "hA", "cluster/b.md": "hB",
	} {
		if got, ok := merged[subpath]; !ok || got.Hash != wantHash {
			t.Errorf("subtree map lost %s: got %+v (map %v)", subpath, got, merged)
		}
	}
}

// The second merge into a parent costs no extra READ: the run already knows what it
// wrote there, so re-reading would only expose it to serving a stale copy of its own
// write.
func TestSubtreeMergeDoesNotRereadWhatItJustWrote(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)
	ctx := context.Background()

	for _, sub := range []struct{ node, id, hash string }{
		{"node:cluster/a.md", "page-a", "hA"},
		{"node:cluster/b.md", "page-b", "hB"},
	} {
		prov := publish.Provenance{Nodes: map[publish.SymbolicID]publish.NodeProvenance{
			publish.SymbolicID(sub.node): {
				ID: publish.BackendID(sub.id), OwnerID: "page-parent",
				NodeStamp: publish.NodeStamp{Hash: publish.Hash(sub.hash), Parent: "node:cluster/index.md"},
			},
		}}
		if err := be.WriteBack(ctx, prov); err != nil {
			t.Fatalf("write-back %s: %v", sub.node, err)
		}
	}

	if got := f.countPath(http.MethodGet, "/pages/page-parent"); got != 1 {
		t.Errorf("parent row read %d time(s), want 1 — the run remembers its own write", got)
	}
}

// The end-to-end shape of #135: a publish that dies partway leaves the pages it
// finished DESCRIBING THEMSELVES in the mirror — path recorded at create, hash
// recorded by that group's write-back — so the next run's scan reads the truth
// rather than state predating every write the run made.
func TestInterruptedPublishLeavesFinishedPagesDescribed(t *testing.T) {
	f := newFakeNotion()
	// Fail the run partway: the fake rejects the SECOND page create, standing in for
	// a cancelled job or a rejected write.
	f.failCreateFrom = 2
	be := newServer(t, f, WithLogger(func(string, ...any) {}))

	g := &graph.Graph{Ops: []*graph.Op{
		createOp("node:first.md"), propsOp("node:first.md"),
		contentOpBlocks("node:first.md", publish.Block{Content: para(txt("one"))}),
		createOp("node:second.md"), propsOp("node:second.md"),
		contentOpBlocks("node:second.md", publish.Block{Content: para(txt("two"))}),
	}}
	for _, op := range g.Ops {
		op.NodeStamp = publish.NodeStamp{Hash: publish.Hash("h-" + string(op.Node))}
	}

	dag := optimize.Optimize(g, be, be)
	if _, err := transport.New(be).Run(context.Background(), dag, publish.NewCurrentState(nil, nil, nil)); err == nil {
		t.Fatal("publish should have failed on the second create")
	}

	// The first page was created carrying its path...
	creates := f.requestsTo(http.MethodPost, "/pages")
	if len(creates) == 0 {
		t.Fatal("no page was created")
	}
	props := digInto(t, creates[0].Body, "properties")
	if got := plainTextOf(t, props["path"]); got != "first.md" {
		t.Errorf("created row carries path %q, want first.md", got)
	}

	// ...and its group's write-back recorded the hash before the run died.
	var recorded bool
	for _, req := range f.reqs {
		if req.Method != http.MethodPatch || !strings.HasPrefix(req.Path, "/pages/") {
			continue
		}
		p, ok := req.Body["properties"].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := p["hash"]; ok {
			recorded = true
		}
	}
	if !recorded {
		t.Error("the finished page's hash was never recorded — an interrupted run recorded nothing")
	}
}
