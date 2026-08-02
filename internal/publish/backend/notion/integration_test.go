package notion

import (
	"context"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/optimize"
	"github.com/sigma/okf-tools/internal/publish/transport"
)

// contentBlocks wraps neutral blocks into a SetContent op for node.
func contentOpBlocks(node publish.SymbolicID, blocks ...publish.Block) *graph.Op {
	return &graph.Op{Kind: graph.SetContent, Node: node, Doc: &publish.Document{
		Group: publish.GroupKey(node), Blocks: blocks,
	}}
}

// TestNotionEndToEnd drives the whole publish — Stage-2 optimize with the real
// Notion Tokenizer + ConstraintModel, then Stage-3 transport — against a faked
// Notion API surface, offline. It exercises every mechanism the seam must
// sequence: a top-level glossary that hosts an anchor, a page that both links a
// sibling (content-refs-node) and cites the glossary anchor (content-refs-anchor),
// and a parent-before-child create. It asserts the recorded API calls and that
// every node and the anchor resolved through the transport's table.
func TestNotionEndToEnd(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	g := &graph.Graph{Ops: []*graph.Op{
		// index.md — top-level parent.
		createOp("node:index.md"), propsOp("node:index.md"),
		contentOpBlocks("node:index.md", publish.Block{Content: para(txt("Root"))}),

		// glossary.md — top-level; its content block hosts the anchor.
		createOp("node:glossary.md"), propsOp("node:glossary.md"),
		contentOpBlocks("node:glossary.md", publish.Block{
			Content: para(txt("Root KEK")),
			Anchors: []publish.AnchorName{"glossary/root-kek"},
		}),

		// b.md — child of index, a plain page (the cross-link target).
		{Kind: graph.CreateNode, Node: "node:b.md", Parent: "node:index.md"},
		propsOp("node:b.md"),
		contentOpBlocks("node:b.md", publish.Block{Content: para(txt("Bee"))}),

		// a.md — child of index; links b.md and cites the glossary anchor.
		{Kind: graph.CreateNode, Node: "node:a.md", Parent: "node:index.md"},
		propsOp("node:a.md"),
		contentOpBlocks("node:a.md", publish.Block{
			Content: para(txt("see "), ref("node:b.md"), txt(" and "), ref("anchor:glossary/root-kek")),
		}),
	}}

	// Unbounded Notion bins: each node's create+props+content fuse into one POST
	// /pages, so acyclic ordering runs purely through cross-node / anchor edges.
	dag := optimize.Optimize(g, be, be)

	seed := publish.NewCurrentState(nil, nil, nil) // empty scan: everything is new
	res, err := transport.New(be, transport.WithInterval(0)).Run(context.Background(), dag, seed)
	if err != nil {
		t.Fatalf("transport run: %v", err)
	}

	// Every source node created this run resolved to a minted page id.
	for _, n := range []publish.SymbolicID{
		"node:index.md", "node:glossary.md", "node:a.md", "node:b.md",
	} {
		if _, ok := res.Nodes[n]; !ok {
			t.Errorf("node %s never resolved in the final table", n)
		}
	}
	// The glossary anchor the page cites was hosted and resolved.
	if _, ok := res.Anchors["glossary/root-kek"]; !ok {
		t.Errorf("anchor glossary/root-kek never resolved, anchors = %v", res.Anchors)
	}

	// Four fused creates, one POST /pages each.
	if got := f.countPath("POST", "/pages"); got != 4 {
		t.Errorf("want 4 POST /pages (one fused create per node), got %d", got)
	}

	// a.md's fused create carries the anchor mention resolved to the glossary's
	// hosting block id — proof the anchor edge sequenced correctly and the swap
	// happened behind the seam.
	anchorID := string(res.Anchors["glossary/root-kek"])
	if anchorID == "" {
		t.Fatal("no anchor id to check content substitution against")
	}
	if !bodyMentions(f.requestsTo("POST", "/pages"), anchorID) {
		t.Errorf("no POST /pages body mentions the resolved anchor id %s", anchorID)
	}
}

// bodyMentions reports whether any recorded create carried a page mention with the
// given resolved id in one of its children.
func bodyMentions(reqs []recordedReq, id string) bool {
	for _, req := range reqs {
		children, _ := req.Body["children"].([]any)
		for _, c := range children {
			block, _ := c.(map[string]any)
			para, _ := block["paragraph"].(map[string]any)
			rich, _ := para["rich_text"].([]any)
			for _, run := range rich {
				m, _ := run.(map[string]any)
				mention, _ := m["mention"].(map[string]any)
				page, _ := mention["page"].(map[string]any)
				if page["id"] == id {
					return true
				}
			}
		}
	}
	return false
}
