package notion

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/optimize"
	"github.com/sigma/okf-tools/internal/publish/transport"
)

// TestExecuteContentAssertionClearsExistingChildren reproduces sigma/okf-tools#130:
// a content assertion against a page that already has a body must REPLACE it, not
// append a second copy. The recorded request stream must show every existing child
// deleted before the new blocks are appended.
func TestExecuteContentAssertionClearsExistingChildren(t *testing.T) {
	f := newFakeNotion()
	f.children["page-a-real"] = []string{"block-old-1", "block-old-2"}
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:a.md", AssertsContent: true,
		Children: []childBlock{paraBlock("fresh body")},
	}
	r := stubResolver{"node:a.md": "page-a-real"}
	if _, err := be.Execute(context.Background(), txn, r); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	deleted := f.deletedBlockIDs()
	if !slices.Equal(deleted, []string{"block-old-1", "block-old-2"}) {
		t.Errorf("want both existing children deleted, deleted = %v", deleted)
	}
	appendAt := f.pathIndex("PATCH", "/blocks/page-a-real/children")
	if appendAt < 0 {
		t.Fatalf("want an append after the clear, requests = %v", f.reqs)
	}
	for _, id := range []string{"block-old-1", "block-old-2"} {
		if at := f.pathIndex("DELETE", "/blocks/"+id); at < 0 || at > appendAt {
			t.Errorf("delete of %s (index %d) must precede the append (index %d)", id, at, appendAt)
		}
	}
	if got := f.children["page-a-real"]; len(got) != 1 {
		t.Errorf("page should hold exactly the transaction's blocks, got %v", got)
	}
}

// TestExecuteContinuationAppendKeepsExistingChildren guards the overflow property:
// the second and later chunks of ONE node's content are continuations, not fresh
// assertions. They must append without clearing, or a large page would end up
// holding only its last chunk.
func TestExecuteContinuationAppendKeepsExistingChildren(t *testing.T) {
	f := newFakeNotion()
	f.children["page-a-real"] = []string{"block-chunk-1"}
	be := newServer(t, f)

	txn := &Transaction{Group: "node:a.md", Children: []childBlock{paraBlock("tail")}}
	r := stubResolver{"node:a.md": "page-a-real"}
	if _, err := be.Execute(context.Background(), txn, r); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if deleted := f.deletedBlockIDs(); len(deleted) != 0 {
		t.Errorf("a continuation append must not clear the page, deleted = %v", deleted)
	}
	if f.countPath("GET", "/blocks/page-a-real/children") != 0 {
		t.Errorf("a continuation append should not even list the page's children")
	}
	if got := f.children["page-a-real"]; len(got) != 2 || got[0] != "block-chunk-1" {
		t.Errorf("continuation should extend the previous chunk, got %v", got)
	}
}

// TestExecuteReplacementRebuildsAnchorMap: replacing a page's content mints new
// block ids, so a self-hosted anchor must map to the NEWLY appended block, never to
// an id carried over from the blocks the replacement destroyed.
func TestExecuteReplacementRebuildsAnchorMap(t *testing.T) {
	f := newFakeNotion()
	f.children["page-g-real"] = []string{"block-stale-anchor"}
	be := newServer(t, f)

	hosting := paraBlock("Root KEK")
	hosting.anchors = []publish.AnchorName{"glossary/root-kek"}
	txn := &Transaction{
		Group: "node:glossary.md", AssertsContent: true,
		Children: []childBlock{hosting},
	}
	r := stubResolver{
		"node:glossary.md":         "page-g-real",
		"anchor:glossary/root-kek": "block-stale-anchor", // the scan seed, now deleted
	}
	res, err := be.Execute(context.Background(), txn, r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, ok := res.Anchors["glossary/root-kek"]
	if !ok || got == "" {
		t.Fatalf("the anchor should be remapped onto the replacement block, got %v", res.Anchors)
	}
	if got == "block-stale-anchor" {
		t.Errorf("the anchor must not keep the destroyed block's id, got %q", got)
	}
	if live := f.children["page-g-real"]; !slices.Equal(live, []string{string(got)}) {
		t.Errorf("the anchor should point at the page's one live child, children = %v, anchor = %q", live, got)
	}
}

// TestExecuteReplacementSpareChildPages: a cluster index page carries its subpages
// as `child_page` children. Those are nodes in their own right, not the index's
// content — the same rule that excludes a child_page from a page's content hash —
// so a content assertion against the index must leave them alone. Deleting them
// would archive whole pages, and the parent row's `hashes` subtree would still
// record them, so no later run would notice.
func TestExecuteReplacementSparesChildPages(t *testing.T) {
	f := newFakeNotion()
	f.children["page-index-real"] = []string{"block-old-body", "page-sub-real"}
	f.liveBlocks["page-index-real"] = []map[string]any{
		paraLive("block-old-body", "stale body"),
		childPageLive("page-sub-real", "Subpage"),
	}
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:index.md", AssertsContent: true,
		Children: []childBlock{paraBlock("fresh body")},
	}
	r := stubResolver{"node:index.md": "page-index-real"}
	if _, err := be.Execute(context.Background(), txn, r); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	deleted := f.deletedBlockIDs()
	if !slices.Equal(deleted, []string{"block-old-body"}) {
		t.Errorf("only the index's own content should be cleared, deleted = %v", deleted)
	}
	if !slices.Contains(f.children["page-index-real"], "page-sub-real") {
		t.Errorf("the subpage must survive its parent's content assertion, children = %v", f.children["page-index-real"])
	}
}

// TestFirstContentChunkAssertsRestContinue: the distinction the replacement rests
// on is carried on the transaction. The tokenizer marks a document's first block,
// and the bin promotes it to AssertsContent — so a node's first content transaction
// asserts its whole body while its overflow chunks merely continue it.
func TestFirstContentChunkAssertsRestContinue(t *testing.T) {
	be := New(WithMaxBlocksPerTxn(1))
	doc := publish.Document{Group: "node:a.md", Blocks: []publish.Block{
		{Content: para(txt("one"))},
		{Content: para(txt("two"))},
	}}
	units := be.Tokenize(doc)
	if len(units) != 2 {
		t.Fatalf("want 2 units, got %d", len(units))
	}

	first := be.NewBin()
	if !first.Add(units[0]) {
		t.Fatal("the first unit must fit an empty bin")
	}
	if first.Add(units[1]) {
		t.Fatal("maxBlocks=1 should refuse the second unit")
	}
	second := be.NewBin()
	if !second.Add(units[1]) {
		t.Fatal("the second unit must fit a fresh bin")
	}

	if t1 := first.Build().(*Transaction); !t1.AssertsContent {
		t.Error("the transaction carrying a node's first content block should assert (replace) its content")
	}
	if t2 := second.Build().(*Transaction); t2.AssertsContent {
		t.Error("an overflow chunk is a continuation and must not clear the page")
	}
}

// TestNotionRepublishIsIdempotent drives the whole publish twice against one faked
// workspace for a node that already exists in the mirror (scan-seeded, no create).
// Two runs asserting the same content must leave the page in the same state — the
// property that makes the stored content hash a meaningful change signal.
//
// "Same state" is checked as block COUNT plus the disappearance of everything that
// was there before: Notion mints a fresh id per appended block, so the two runs'
// literal id lists necessarily differ. The bug this guards is a page that grows —
// 2 stale blocks, then 5, then 8 — so the count is the signal.
func TestNotionRepublishIsIdempotent(t *testing.T) {
	f := newFakeNotion()
	f.children["page-a-real"] = []string{"block-published-1", "block-published-2"}
	be := newServer(t, f)

	g := &graph.Graph{Ops: []*graph.Op{
		contentOpBlocks("node:a.md",
			publish.Block{Content: para(txt("first"))},
			publish.Block{Content: para(txt("second"))},
			publish.Block{Content: para(txt("third"))},
		),
	}}
	seed := publish.NewCurrentState(
		map[publish.SymbolicID]publish.BackendID{"node:a.md": "page-a-real"}, nil, nil)

	var after [2][]string
	for run := range after {
		dag := optimize.Optimize(g, be, be)
		if _, err := transport.New(be).Run(context.Background(), dag, seed); err != nil {
			t.Fatalf("run %d: %v", run+1, err)
		}
		after[run] = slices.Clone(f.children["page-a-real"])
		if len(after[run]) != 3 {
			t.Errorf("run %d: want the page to hold exactly the 3 asserted blocks, got %v", run+1, after[run])
		}
	}
	for _, stale := range []string{"block-published-1", "block-published-2"} {
		if slices.Contains(after[1], stale) {
			t.Errorf("the pre-existing body should have been replaced, %s survives in %v", stale, after[1])
		}
	}
	if slices.ContainsFunc(after[1], func(id string) bool { return slices.Contains(after[0], id) }) {
		t.Errorf("run 2 asserted the content afresh, so no block of run 1 should survive: %v then %v", after[0], after[1])
	}
}

// TestNotionRepublishWithBlockedHeadChunkKeepsEveryBlock is the end-to-end form of
// the ordering hazard replacement introduces. A re-published page overflows into two
// content transactions, and its FIRST chunk links a page created this run — so on
// refs alone the head is blocked while the tail is ready. If the tail ran first, the
// head's assertion would then clear the page and delete it: the page would end up
// holding only its head. Every block must survive, in source order.
func TestNotionRepublishWithBlockedHeadChunkKeepsEveryBlock(t *testing.T) {
	f := newFakeNotion()
	f.children["page-a-real"] = []string{"block-published-1"}
	be := newServer(t, f, WithMaxBlocksPerTxn(2))

	g := &graph.Graph{Ops: []*graph.Op{
		// a.md already exists; its head links zz.md, which this run creates.
		contentOpBlocks("node:a.md",
			publish.Block{Content: para(txt("see "), ref("node:zz.md"))},
			publish.Block{Content: para(txt("middle"))},
			publish.Block{Content: para(txt("tail"))},
		),
		createOp("node:zz.md"), propsOp("node:zz.md"),
		contentOpBlocks("node:zz.md", publish.Block{Content: para(txt("Zed"))}),
	}}
	seed := publish.NewCurrentState(
		map[publish.SymbolicID]publish.BackendID{"node:a.md": "page-a-real"}, nil, nil)

	dag := optimize.Optimize(g, be, be)
	if _, err := transport.New(be).Run(context.Background(), dag, seed); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := f.children["page-a-real"]
	if len(got) != 3 {
		t.Fatalf("want all 3 asserted blocks on the page, got %d: %v", len(got), got)
	}
	if slices.Contains(got, "block-published-1") {
		t.Errorf("the previous body should have been replaced, got %v", got)
	}
	// Block ids are minted in append order, so ascending ids prove source order held.
	if !slices.IsSorted(got) {
		t.Errorf("the page's blocks should be in source order, got %v", got)
	}
}

// TestNotionOverflowingCreateKeepsEveryChunk guards the overflow property end to
// end: a create whose content exceeds one request appends the remainder in follow-on
// transactions, and those continuations must not each clear the page — the whole
// block list must survive.
func TestNotionOverflowingCreateKeepsEveryChunk(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f, WithMaxBlocksPerTxn(2))

	blocks := make([]publish.Block, 5)
	for i := range blocks {
		blocks[i] = publish.Block{Content: para(txt(strings.Repeat("x", i+1)))}
	}
	g := &graph.Graph{Ops: []*graph.Op{
		createOp("node:a.md"), propsOp("node:a.md"),
		contentOpBlocks("node:a.md", blocks...),
	}}

	dag := optimize.Optimize(g, be, be)
	if _, err := transport.New(be).Run(context.Background(), dag, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := f.children["page-1"]; len(got) != len(blocks) {
		t.Errorf("want all %d blocks on the page, got %d: %v", len(blocks), len(got), got)
	}
	if deleted := f.deletedBlockIDs(); len(deleted) != 0 {
		t.Errorf("a fresh create has nothing to replace, deleted = %v", deleted)
	}
}
