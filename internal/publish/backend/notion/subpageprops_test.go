package notion

import (
	"context"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/optimize"
)

// TestSubpagePropsOnlyUpdateSurvivesMissingPropHash drives the exact trigger of
// sigma/okf-tools#128 end to end: scan → generate → optimize → execute.
//
// A cluster subpage has no data-source row; its {id, hash} lives in the parent row's
// subtree map, whose per-entry property hash is `omitempty` and postdates the #110
// phase-2 two-hash split. So every entry written before phase 2 seeds NO property
// hash, which leaves the SetProperties arm ungated and re-asserts it on every run —
// while the content arm hash-skips. That standalone property update used to PATCH
// the data source's columns onto a child_page and 400 "Invalid property identifier",
// aborting the whole drain (and with it write-back, which is #125's mechanism).
func TestSubpagePropsOnlyUpdateSurvivesMissingPropHash(t *testing.T) {
	b := loadTestBundle(t, map[string]string{
		"okf.toml":      "",
		"docs/index.md": "# Docs\n",
		"docs/a.md": "---\ntype: note\nstatus: draft\ncreated: \"2026-01-01\"\ntitle: A\n---\n" +
			"Body of A.\n",
	})
	index, child := docByRel(t, b, "docs/index.md"), docByRel(t, b, "docs/a.md")

	// The stored mirror: docs/index.md is the top-level row (nothing indexes above
	// it), docs/a.md nests under it as a child_page recorded in the row's subtree
	// map — with its content hash but, crucially, no `prop_hash`.
	subtree := mustJSON(t, map[string]subtreeEntry{
		child.Rel: {ID: "page-child-real", Hash: string(graph.ContentHash(child)), Title: child.Title()},
	})
	f := newFakeNotion()
	f.childPages["page-child-real"] = true
	f.rows = []map[string]any{row("page-index-real", map[string]any{
		"path":   richProp(index.Rel),
		"hash":   richProp(encodeHashPair(graph.ContentHash(index), graph.PropertyHash(index))),
		"hashes": richProp(subtree),
	})}
	be := newServer(t, f)

	cs, err := be.Scan(context.Background(), backend.ScanStored)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if _, ok := cs.PropertyHash(publish.NodeRef(child.Rel)); ok {
		t.Fatalf("the seeded subtree entry must carry no property hash — that is the trigger")
	}

	g, err := graph.Generate(context.Background(), b, cs)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// The trigger itself: the missing property hash leaves the subpage's
	// SetProperties ungated, while its unchanged content hash-skips.
	var kinds []graph.OpKind
	for _, op := range g.Ops {
		if op.Node == publish.NodeRef(child.Rel) {
			kinds = append(kinds, op.Kind)
		}
	}
	if len(kinds) != 1 || kinds[0] != graph.SetProperties {
		t.Fatalf("subpage ops = %v, want a standalone [SetProperties]", kinds)
	}

	// Execute that standalone property update against the faked Notion surface,
	// which rejects a column property on a child_page exactly as Notion does.
	d := optimize.Optimize(g, be, be)
	r := stubResolver{
		publish.NodeRef(index.Rel): "page-index-real",
		publish.NodeRef(child.Rel): "page-child-real",
	}
	executed := 0
	for _, pt := range d.Txns {
		if pt.Group != publish.GroupKey(publish.NodeRef(child.Rel)) {
			continue
		}
		if _, err := be.Execute(context.Background(), pt.Txn, r); err != nil {
			t.Fatalf("executing the subpage property update: %v", err)
		}
		executed++
	}
	if executed != 1 {
		t.Fatalf("want 1 transaction for the subpage, got %d", executed)
	}

	reqs := f.requestsTo("PATCH", "/pages/page-child-real")
	if len(reqs) != 1 {
		t.Fatalf("want 1 property PATCH against the subpage, got %d", len(reqs))
	}
	props := digInto(t, reqs[0].Body, "properties")
	if len(props) != 1 {
		t.Errorf("subpage update must carry only the title, got %v", props)
	}
	if _, ok := props["title"]; !ok {
		t.Errorf("subpage update should carry the title, got %v", props)
	}
}

// TestOptimizeCarriesParentOntoPropsOnlyTransaction pins the plumbing the rule
// depends on: the neutral SetProperties op stamps the node's parent, and it survives
// tokenization and packing onto a props-only Notion Transaction — the information
// the update path used to drop. A top-level node's props-only transaction carries no
// parent, so it still writes the full column set.
func TestOptimizeCarriesParentOntoPropsOnlyTransaction(t *testing.T) {
	be := New()
	g := &graph.Graph{Ops: []*graph.Op{
		{
			Kind: graph.SetProperties, Node: "node:docs/a.md",
			Props:     map[string]any{"title": "A"},
			NodeStamp: publish.NodeStamp{Parent: "node:docs/index.md"},
		},
		{
			Kind: graph.SetProperties, Node: "node:top.md",
			Props: map[string]any{"title": "Top"},
		},
	}}

	byGroup := map[publish.GroupKey]*Transaction{}
	for _, pt := range optimize.Optimize(g, be, be).Txns {
		byGroup[pt.Group] = pt.Txn.(*Transaction)
	}
	if got := byGroup["node:docs/a.md"]; got == nil || got.Parent != "node:docs/index.md" {
		t.Errorf("a page-parented props-only txn should carry its parent, got %+v", got)
	}
	if got := byGroup["node:top.md"]; got == nil || got.Parent != "" {
		t.Errorf("a top-level props-only txn should carry no parent, got %+v", got)
	}
}
