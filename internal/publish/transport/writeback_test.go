package transport

import (
	"context"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend/fake"
	"github.com/sigma/okf-tools/internal/publish/optimize"
)

// TestTransportWritesBackProvenance: after a successful drain the transport hands
// the backend the run's provenance — resolved node ids, expected hashes, hosted
// anchors, and subpage→parent routing — assembled from the executed transactions
// and the resolution table.
func TestTransportWritesBackProvenance(t *testing.T) {
	// Seed the scan with an existing parent row so a subpage's parent resolves from
	// the seed (it is not created this run).
	seed := publish.NewCurrentState(
		map[publish.SymbolicID]publish.BackendID{"node:index.md": "page-index"},
		nil, nil,
	)
	be := fake.New(fake.WithScan(seed))

	// A glossary top-level node (hosts an anchor) and a subpage under the seeded
	// parent. Each is its own transaction; the fake mints ids at Execute.
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		{
			Txn:       fakeTxn(be, "node:CONTEXT.md", "glossary/root-kek"),
			Group:     "node:CONTEXT.md",
			Produces:  []publish.SymbolicID{"node:CONTEXT.md"},
			Anchors:   []publish.AnchorName{"glossary/root-kek"},
			NodeStamp: publish.NodeStamp{Hash: "hG"},
		},
		{
			Txn:       fakeTxn(be, "node:docs/adr/sub.md"),
			Group:     "node:docs/adr/sub.md",
			Produces:  []publish.SymbolicID{"node:docs/adr/sub.md"},
			NodeStamp: publish.NodeStamp{Hash: "hS", Parent: "node:index.md", Title: "Sub Page"},
		},
	}}

	if _, err := New(be, WithInterval(0)).Run(context.Background(), dag, seed); err != nil {
		t.Fatalf("Run: %v", err)
	}

	written := be.WrittenBack()
	if len(written) != 1 {
		t.Fatalf("want exactly one write-back, got %d", len(written))
	}
	nodes := written[0].Nodes

	g, ok := nodes["node:CONTEXT.md"]
	if !ok {
		t.Fatalf("write-back missing the glossary node: %v", nodes)
	}
	if g.Hash != "hG" || g.Parent != "" {
		t.Errorf("glossary provenance = %+v, want hash hG and no parent", g)
	}
	if g.ID == "" {
		t.Errorf("glossary provenance should carry its resolved id")
	}
	if id, ok := g.Anchors["glossary/root-kek"]; !ok || id == "" {
		t.Errorf("glossary provenance should carry the hosted anchor id, got %v", g.Anchors)
	}

	sub, ok := nodes["node:docs/adr/sub.md"]
	if !ok {
		t.Fatalf("write-back missing the subpage node: %v", nodes)
	}
	if sub.Hash != "hS" || sub.Parent != "node:index.md" || sub.Title != "Sub Page" {
		t.Errorf("subpage provenance = %+v, want hash hS, parent node:index.md, title Sub Page", sub)
	}
	if sub.ParentID != "page-index" {
		t.Errorf("subpage ParentID = %q, want the seed-resolved page-index", sub.ParentID)
	}
}

// TestTransportNoWriteBackWhenNothingWritten: a DeleteNode-only run (no content
// hashes) records no node provenance, so the transport does not call WriteBack —
// the near-noop / archive-only path leaves the mirror's columns untouched.
func TestTransportNoWriteBackWhenNothingWritten(t *testing.T) {
	seed := publish.NewCurrentState(
		map[publish.SymbolicID]publish.BackendID{"node:old.md": "page-old"},
		nil, nil,
	)
	be := fake.New(fake.WithScan(seed))
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		{
			Txn:   fakeTxn(be, "node:old.md"),
			Group: "node:old.md",
			Refs:  []publish.SymbolicID{"node:old.md"}, // scan-seeded archive target
			// no Hash: a DeleteNode writes no content.
		},
	}}

	if _, err := New(be, WithInterval(0)).Run(context.Background(), dag, seed); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := be.WrittenBack(); len(got) != 0 {
		t.Errorf("archive-only run should write nothing back, got %d write-backs", len(got))
	}
}

// fakeTxn seals a one-unit transaction on the fake backend for the given node group
// and optional hosted anchors, so Execute mints the node (and anchor) ids.
func fakeTxn(be *fake.Backend, group publish.GroupKey, anchors ...publish.AnchorName) publish.Transaction {
	bin := be.NewBin()
	bin.Add(publish.AtomicUnit{Group: group, Anchors: anchors})
	return bin.Build()
}
