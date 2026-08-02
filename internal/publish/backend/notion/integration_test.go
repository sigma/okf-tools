package notion

import (
	"context"
	"strings"
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

// TestNotionEndToEndSelfHostedAnchor drives the whole publish for the #89 case: a
// glossary host that both defines an anchor and cites its own anchor, published to
// an empty mirror (first publish, no scan-seeded anchor). This is the offline
// analog of `okfpub run --backend notion` against a verified-empty data source.
// The optimizer suppresses the self-hosted ref from the transport gate, so the
// create is scheduled; the executor must then defer the citation out of the POST
// and patch it in once the anchor block id exists, rather than fail with
// "content ref … did not resolve". The `fake` backend masks this by honoring
// intra-txn self-anchor resolution, so the regression lives on the notion path.
func TestNotionEndToEndSelfHostedAnchor(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	g := &graph.Graph{Ops: []*graph.Op{
		createOp("node:CONTEXT.md"), propsOp("node:CONTEXT.md"),
		// One node, two content blocks: the first hosts the anchor, the second cites
		// that same anchor — the self-hosted anchor pattern on one glossary host.
		contentOpBlocks("node:CONTEXT.md",
			publish.Block{
				Content: para(txt("Emergency block: a fast, time-boxed suspension")),
				Anchors: []publish.AnchorName{"glossary/emergency-block"},
			},
			publish.Block{
				Content: para(txt("held while an "), ref("anchor:glossary/emergency-block")),
			},
		),
	}}

	dag := optimize.Optimize(g, be, be)
	seed := publish.NewCurrentState(nil, nil, nil) // empty mirror: first publish
	res, err := transport.New(be, transport.WithInterval(0)).Run(context.Background(), dag, seed)
	if err != nil {
		t.Fatalf("first publish of a self-hosting glossary to an empty mirror should succeed: %v", err)
	}

	anchorID := string(res.Anchors["glossary/emergency-block"])
	if anchorID == "" {
		t.Fatalf("the self-hosted anchor never resolved, anchors = %v", res.Anchors)
	}

	// The citation was deferred out of the create and re-materialized by an in-place
	// block patch carrying the resolved anchor block id.
	patch, ok := blockContentPatchMentioning(f.reqs, anchorID)
	if !ok {
		t.Fatalf("no in-place block patch mentions the resolved anchor id %s; reqs = %v", anchorID, f.reqs)
	}
	if patch != "PATCH /blocks/"+anchorID {
		// The self-defining, self-citing case: the citing run lives in a different
		// block than the host, so the patched block id differs from the anchor id.
		// Both are valid; this branch just records which happened.
		t.Logf("self-cite patched on %s (anchor hosted on block %s)", patch, anchorID)
	}
}

// blockContentPatchMentioning finds a PATCH /blocks/{id} (an in-place block content
// update, not a /children append) whose body mentions the given page id, returning
// its "METHOD path" and whether one was found.
func blockContentPatchMentioning(reqs []recordedReq, id string) (string, bool) {
	for _, req := range reqs {
		if req.Method != "PATCH" || strings.HasSuffix(req.Path, "/children") {
			continue
		}
		if !strings.HasPrefix(req.Path, "/blocks/") {
			continue
		}
		para, _ := req.Body["paragraph"].(map[string]any)
		rich, _ := para["rich_text"].([]any)
		for _, run := range rich {
			m, _ := run.(map[string]any)
			mention, _ := m["mention"].(map[string]any)
			page, _ := mention["page"].(map[string]any)
			if page["id"] == id {
				return req.Method + " " + req.Path, true
			}
		}
	}
	return "", false
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
