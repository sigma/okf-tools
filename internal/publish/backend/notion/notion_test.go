package notion

import (
	"slices"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/optimize"
)

// --- helpers ----------------------------------------------------------------

func para(inlines ...graph.Inline) graph.BlockContent {
	return graph.BlockContent{Kind: graph.Paragraph, Inlines: inlines}
}
func txt(s string) graph.Inline              { return graph.Inline{Text: s} }
func ref(id publish.SymbolicID) graph.Inline { return graph.Inline{Ref: &graph.Ref{ID: id}} }
func runsText(cb childBlock) string {
	var b strings.Builder
	for _, r := range cb.runs {
		b.WriteString(r.Text)
	}
	return b.String()
}
func payload(t *testing.T, u publish.AtomicUnit) childBlock {
	t.Helper()
	cb, ok := u.Payload.(childBlock)
	if !ok {
		t.Fatalf("unit Payload is %T, want childBlock", u.Payload)
	}
	return cb
}

// --- Tokenizer: char-cap splitting ------------------------------------------

// A block whose text exceeds the per-block char cap is split into several units
// during tokenization, each within the cap, each one Notion block (Cost 1), and
// the concatenation reproduces the original text.
func TestTokenizeSplitsOversizedBlock(t *testing.T) {
	b := New(WithMaxBlockChars(5))
	doc := publish.Document{Group: "node:a.md", Blocks: []publish.Block{
		{Content: para(txt("abcdefghij"))}, // 10 chars → 2 units of 5
	}}

	units := b.Tokenize(doc)
	if len(units) != 2 {
		t.Fatalf("got %d units, want 2 (10 chars / cap 5)", len(units))
	}
	var whole string
	for i, u := range units {
		if u.Cost != 1 {
			t.Errorf("unit %d Cost = %v, want 1", i, u.Cost)
		}
		if u.Group != "node:a.md" {
			t.Errorf("unit %d Group = %q, want the document group", i, u.Group)
		}
		cb := payload(t, u)
		if got := len([]rune(runsText(cb))); got > 5 {
			t.Errorf("unit %d holds %d chars, over the cap of 5", i, got)
		}
		whole += runsText(cb)
	}
	if whole != "abcdefghij" {
		t.Errorf("reassembled text = %q, want the original", whole)
	}
}

// A block within the cap is a single unit; the cap only splits oversized content.
func TestTokenizeSmallBlockStaysWhole(t *testing.T) {
	b := New() // default 2000-char cap
	doc := publish.Document{Group: "node:a.md", Blocks: []publish.Block{
		{Content: para(txt("short"))},
	}}
	if units := b.Tokenize(doc); len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
}

// An empty block still emits exactly one unit (an empty Notion block).
func TestTokenizeEmptyBlockEmitsOneUnit(t *testing.T) {
	b := New()
	doc := publish.Document{Group: "node:a.md", Blocks: []publish.Block{{Content: nil}}}
	if units := b.Tokenize(doc); len(units) != 1 {
		t.Fatalf("got %d units, want 1 for an empty block", len(units))
	}
}

// --- Tokenizer: refs and the anchor map -------------------------------------

// Inline Refs are preserved on the unit that carries them; a block's declared
// anchors are reported on the first unit (the block that hosts the anchor id).
func TestTokenizePreservesRefsAndAnchors(t *testing.T) {
	b := New()
	doc := publish.Document{Group: "node:a.md", Blocks: []publish.Block{
		{
			Content: para(txt("see "), ref("node:b.md"), txt(" and "), ref("anchor:glossary/term")),
			Anchors: []publish.AnchorName{"glossary/host"},
		},
	}}

	units := b.Tokenize(doc)
	if len(units) != 1 {
		t.Fatalf("got %d units, want 1", len(units))
	}
	wantRefs := []publish.SymbolicID{"node:b.md", "anchor:glossary/term"}
	if !slices.Equal(units[0].Refs, wantRefs) {
		t.Errorf("Refs = %v, want %v preserved in order", units[0].Refs, wantRefs)
	}
	if got := units[0].Anchors; len(got) != 1 || got[0] != "glossary/host" {
		t.Errorf("Anchors = %v, want [glossary/host] on the hosting unit", got)
	}
}

// When an oversized block splits, each Ref rides the split unit that contains it,
// and the anchor stays on the first unit only.
func TestTokenizeSplitDistributesRefsAndAnchor(t *testing.T) {
	b := New(WithMaxBlockChars(5))
	doc := publish.Document{Group: "node:a.md", Blocks: []publish.Block{
		{
			// "aaaaa" fills unit 0; ref rides into unit 0 (curChars==cap, ref costs 0),
			// then "bbbbb" fills unit 1 carrying the second ref.
			Content: para(txt("aaaaa"), ref("node:x.md"), txt("bbbbb"), ref("node:y.md")),
			Anchors: []publish.AnchorName{"glossary/term"},
		},
	}}

	units := b.Tokenize(doc)
	if len(units) != 2 {
		t.Fatalf("got %d units, want 2", len(units))
	}
	if !slices.Contains(units[0].Refs, publish.SymbolicID("node:x.md")) {
		t.Errorf("unit 0 Refs = %v, want to contain node:x.md", units[0].Refs)
	}
	if !slices.Contains(units[1].Refs, publish.SymbolicID("node:y.md")) {
		t.Errorf("unit 1 Refs = %v, want to contain node:y.md", units[1].Refs)
	}
	if len(units[0].Anchors) != 1 {
		t.Errorf("unit 0 should host the anchor, got %v", units[0].Anchors)
	}
	if len(units[1].Anchors) != 0 {
		t.Errorf("only the first unit hosts the anchor, unit 1 = %v", units[1].Anchors)
	}
}

// --- TokenizeOp: the non-content payloads -----------------------------------

func TestTokenizeOpPayloads(t *testing.T) {
	b := New()

	c := b.TokenizeOp(publish.NonContentOp{Kind: publish.CreateOp, Node: "node:a.md"})
	if _, ok := c.Payload.(createBlock); !ok {
		t.Errorf("CreateOp payload = %T, want createBlock", c.Payload)
	}
	if c.Cost != 0 {
		t.Errorf("create Cost = %v, want 0 (no children)", c.Cost)
	}

	props := map[string]any{"title": "A"}
	p := b.TokenizeOp(publish.NonContentOp{Kind: publish.PropertiesOp, Node: "node:a.md", Props: props})
	pb, ok := p.Payload.(propsBlock)
	if !ok {
		t.Fatalf("PropertiesOp payload = %T, want propsBlock", p.Payload)
	}
	if pb.props["title"] != "A" {
		t.Errorf("props payload = %v, want the neutral map carried through", pb.props)
	}

	d := b.TokenizeOp(publish.NonContentOp{Kind: publish.DeleteOp, Node: "node:a.md"})
	if _, ok := d.Payload.(deleteBlock); !ok {
		t.Errorf("DeleteOp payload = %T, want deleteBlock", d.Payload)
	}
}

// --- Bin: fusion, ceilings, group affinity, delete-standalone ---------------

// contentUnits tokenizes n one-line blocks into content units of one group.
func contentUnits(b *Backend, group publish.GroupKey, n int) []publish.AtomicUnit {
	blocks := make([]publish.Block, n)
	for i := range blocks {
		blocks[i] = publish.Block{Content: para(txt("x"))}
	}
	return b.Tokenize(publish.Document{Group: group, Blocks: blocks})
}

func createUnit(b *Backend, node publish.SymbolicID) publish.AtomicUnit {
	u := b.TokenizeOp(publish.NonContentOp{Kind: publish.CreateOp, Node: node})
	u.Group = publish.GroupKey(node)
	return u
}
func propsUnit(b *Backend, node publish.SymbolicID, props map[string]any) publish.AtomicUnit {
	u := b.TokenizeOp(publish.NonContentOp{Kind: publish.PropertiesOp, Node: node, Props: props})
	u.Group = publish.GroupKey(node)
	return u
}
func deleteUnit(b *Backend, node publish.SymbolicID) publish.AtomicUnit {
	u := b.TokenizeOp(publish.NonContentOp{Kind: publish.DeleteOp, Node: node})
	u.Group = publish.GroupKey(node)
	return u
}

// A co-binned create + properties + content collapses into one POST /pages.
func TestBinFusesCreatePropsContent(t *testing.T) {
	b := New()
	bin := b.NewBin()

	if !bin.Add(createUnit(b, "node:a.md")) {
		t.Fatal("create should be accepted")
	}
	if !bin.Add(propsUnit(b, "node:a.md", map[string]any{"title": "A"})) {
		t.Fatal("props should co-bin with the create")
	}
	for i, u := range contentUnits(b, "node:a.md", 2) {
		if !bin.Add(u) {
			t.Fatalf("content unit %d should co-bin", i)
		}
	}

	txn, ok := bin.Build().(*Transaction)
	if !ok {
		t.Fatalf("Build returned %T, want *Transaction", txn)
	}
	if !txn.Create {
		t.Error("fused transaction should be a page-create (POST /pages)")
	}
	if txn.Props["title"] != "A" {
		t.Errorf("fused transaction should carry the properties, got %v", txn.Props)
	}
	if len(txn.Children) != 2 {
		t.Errorf("fused transaction should carry 2 children, got %d", len(txn.Children))
	}
}

// The Bin refuses to co-bin units of a different Group.
func TestBinRefusesAcrossGroup(t *testing.T) {
	b := New()
	bin := b.NewBin()
	if !bin.Add(contentUnits(b, "node:a.md", 1)[0]) {
		t.Fatal("first unit should be accepted")
	}
	if bin.Add(contentUnits(b, "node:b.md", 1)[0]) {
		t.Fatal("a unit of a different Group must be refused")
	}
}

// The block ceiling is enforced O(1)-per-add: create/props cost zero children, so
// with a cap of 2 the third content unit is refused (fusion overflow point).
func TestBinBlockCeilingOverflow(t *testing.T) {
	b := New(WithMaxBlocksPerTxn(2))
	bin := b.NewBin()
	if !bin.Add(createUnit(b, "node:a.md")) || !bin.Add(propsUnit(b, "node:a.md", nil)) {
		t.Fatal("create+props cost no children, must fit under the block cap")
	}
	cu := contentUnits(b, "node:a.md", 3)
	if !bin.Add(cu[0]) || !bin.Add(cu[1]) {
		t.Fatal("first two content blocks fit the cap of 2")
	}
	if bin.Add(cu[2]) {
		t.Fatal("third content block overflows the cap of 2 and must be refused")
	}
}

// A delete unit never co-bins with a create, and nothing joins a delete bin.
func TestBinDeleteStandsAlone(t *testing.T) {
	b := New()

	bin := b.NewBin()
	bin.Add(createUnit(b, "node:a.md"))
	if bin.Add(deleteUnit(b, "node:a.md")) {
		t.Error("a delete must not co-bin with a create")
	}

	del := b.NewBin()
	if !del.Add(deleteUnit(b, "node:old.md")) {
		t.Fatal("a delete should be accepted into an empty bin")
	}
	if del.Add(contentUnits(b, "node:old.md", 1)[0]) {
		t.Error("nothing may co-bin into a delete bin")
	}
	txn := del.Build().(*Transaction)
	if !txn.Delete {
		t.Error("a delete bin should build an archive transaction")
	}
}

// A lone unit always fits a fresh bin — the ConstraintModel contract the optimizer
// relies on (it panics otherwise).
func TestBinAcceptsLoneUnit(t *testing.T) {
	b := New(WithMaxBlocksPerTxn(1))
	bin := b.NewBin()
	if !bin.Add(contentUnits(b, "node:a.md", 1)[0]) {
		t.Fatal("a fresh bin must accept a lone unit")
	}
}

// --- Optimizer-driven: real Notion Tokenizer + ConstraintModel --------------

func createOp(node publish.SymbolicID) *graph.Op {
	return &graph.Op{Kind: graph.CreateNode, Node: node}
}
func propsOp(node publish.SymbolicID) *graph.Op {
	return &graph.Op{Kind: graph.SetProperties, Node: node, Props: map[string]any{"title": "A"}}
}
func contentOp(node publish.SymbolicID, blocks int) *graph.Op {
	bs := make([]publish.Block, blocks)
	for i := range bs {
		bs[i] = publish.Block{Content: para(txt("x"))}
	}
	return &graph.Op{Kind: graph.SetContent, Node: node, Doc: &publish.Document{
		Group: publish.GroupKey(node), Blocks: bs,
	}}
}

// Through the Stage-2 optimizer with the real Notion backend and no packing
// pressure, a node's create + props + content fuse into ONE POST /pages, and the
// node's own write-target Ref is suppressed (no self-edge).
func TestOptimizeNotionFusesWholeNode(t *testing.T) {
	be := New() // Notion defaults: 100-block ceiling
	g := &graph.Graph{Ops: []*graph.Op{
		createOp("node:a.md"), propsOp("node:a.md"), contentOp("node:a.md", 3),
	}}

	d := optimize.Optimize(g, be, be)
	if len(d.Txns) != 1 {
		t.Fatalf("want 1 fused txn, got %d", len(d.Txns))
	}
	if len(d.Txns[0].Refs) != 0 {
		t.Errorf("write-target should be suppressed, exposed Refs = %v", d.Txns[0].Refs)
	}
	txn := d.Txns[0].Txn.(*Transaction)
	if !txn.Create || txn.Props["title"] != "A" || len(txn.Children) != 3 {
		t.Errorf("fused txn = %+v, want create+props+3 children", txn)
	}
	if len(d.Edges) != 0 {
		t.Errorf("a single fused txn has no edges, got %v", d.Edges)
	}
}

// Under packing pressure (block ceiling of 2), the same node's content overflows:
// the create bin (POST /pages with the first 2 children) and a follow-on append
// bin (the third child), with an edge from the create to the overflow bin.
func TestOptimizeNotionOverflowsPastCeiling(t *testing.T) {
	be := New(WithMaxBlocksPerTxn(2))
	g := &graph.Graph{Ops: []*graph.Op{
		createOp("node:a.md"), propsOp("node:a.md"), contentOp("node:a.md", 3),
	}}

	d := optimize.Optimize(g, be, be)
	if len(d.Txns) != 2 {
		t.Fatalf("want 2 txns (fused create-bin + overflow append), got %d", len(d.Txns))
	}

	var createIdx, overflowIdx = -1, -1
	for i, tx := range d.Txns {
		if slices.Contains(tx.Produces, publish.SymbolicID("node:a.md")) {
			createIdx = i
		} else {
			overflowIdx = i
		}
	}
	if createIdx < 0 || overflowIdx < 0 {
		t.Fatalf("could not identify create/overflow txns: %+v", d.Txns)
	}

	createTxn := d.Txns[createIdx].Txn.(*Transaction)
	overflowTxn := d.Txns[overflowIdx].Txn.(*Transaction)
	if !createTxn.Create || len(createTxn.Children) != 2 {
		t.Errorf("create txn should be POST /pages with 2 children, got %+v", createTxn)
	}
	if overflowTxn.Create || len(overflowTxn.Children) != 1 {
		t.Errorf("overflow txn should be a 1-child append (no create), got %+v", overflowTxn)
	}
	if !slices.Contains(d.Txns[overflowIdx].Refs, publish.SymbolicID("node:a.md")) {
		t.Errorf("overflow txn should expose the node write-target, Refs = %v", d.Txns[overflowIdx].Refs)
	}
	if !slices.Contains(d.Edges, optimize.Edge{From: createIdx, To: overflowIdx}) {
		t.Errorf("want overflow edge %d->%d, edges = %v", createIdx, overflowIdx, d.Edges)
	}
}
