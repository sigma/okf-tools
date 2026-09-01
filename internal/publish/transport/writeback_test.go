package transport

import (
	"context"
	"errors"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
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
			NodeStamp: publish.NodeStamp{Hash: "hS", Parent: "node:index.md", Owner: "node:index.md", Title: "Sub Page"},
		},
	}}

	if _, err := New(be).Run(context.Background(), dag, seed); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Write-back is per group (#135), so the run's record is the union of what each
	// completed group persisted; this test is about its CONTENT, not its batching.
	nodes := mergedNodes(be.WrittenBack())

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
	if sub.OwnerID != "page-index" {
		t.Errorf("subpage OwnerID = %q, want the seed-resolved page-index", sub.OwnerID)
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

	if _, err := New(be).Run(context.Background(), dag, seed); err != nil {
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

// --- incremental write-back (#135) ------------------------------------------

// failingBackend wraps the fake and fails the Nth Execute, standing in for a run
// that dies partway — a cancelled CI job, a rejected write, a blocked transaction.
type failingBackend struct {
	*fake.Backend
	failOn int // 1-based index of the Execute call that fails
	calls  int
}

func (f *failingBackend) Execute(ctx context.Context, txn publish.Transaction, r backend.Resolver) (publish.ExecResult, error) {
	f.calls++
	if f.calls == f.failOn {
		return publish.ExecResult{}, errors.New("backend exploded")
	}
	return f.Backend.Execute(ctx, txn, r)
}

// mergedNodes folds every write-back the run made into one view of what the mirror
// was told, so a test can assert on content without caring how it was batched.
func mergedNodes(provs []publish.Provenance) map[publish.SymbolicID]publish.NodeProvenance {
	out := map[publish.SymbolicID]publish.NodeProvenance{}
	for _, p := range provs {
		for node, np := range p.Nodes {
			out[node] = np
		}
	}
	return out
}

// twoGroupDAG is two independent single-transaction groups.
func twoGroupDAG(be *fake.Backend) *optimize.TxnDAG {
	return &optimize.TxnDAG{Txns: []publish.PackedTxn{
		{
			Txn: fakeTxn(be, "node:a.md"), Group: "node:a.md",
			Produces: []publish.SymbolicID{"node:a.md"}, NodeStamp: publish.NodeStamp{Hash: "hA"},
		},
		{
			Txn: fakeTxn(be, "node:b.md"), Group: "node:b.md",
			Produces: []publish.SymbolicID{"node:b.md"}, NodeStamp: publish.NodeStamp{Hash: "hB"},
		},
	}}
}

// Each group's provenance is persisted as that group finishes, not held until the
// whole graph drains — so an interruption cannot discard completed work.
func TestTransportWritesBackPerGroup(t *testing.T) {
	be := fake.New()
	if _, err := New(be).Run(context.Background(), twoGroupDAG(be), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	written := be.WrittenBack()
	if len(written) != 2 {
		t.Fatalf("want one write-back per group (2), got %d: %+v", len(written), written)
	}
	for i, p := range written {
		if len(p.Nodes) != 1 {
			t.Errorf("write-back %d carried %d nodes, want exactly its own group's one", i, len(p.Nodes))
		}
	}
	nodes := mergedNodes(written)
	if _, ok := nodes["node:a.md"]; !ok {
		t.Errorf("node:a.md never recorded: %v", nodes)
	}
	if _, ok := nodes["node:b.md"]; !ok {
		t.Errorf("node:b.md never recorded: %v", nodes)
	}
}

// The headline property of #135: a run that dies partway still leaves the work it
// genuinely completed recorded, so the next run's scan sees the truth.
func TestTransportPersistsCompletedGroupsWhenTheRunFails(t *testing.T) {
	inner := fake.New()
	be := &failingBackend{Backend: inner, failOn: 2}

	_, err := New(be).Run(context.Background(), twoGroupDAG(inner), nil)
	if err == nil {
		t.Fatal("Run should have failed")
	}

	nodes := mergedNodes(inner.WrittenBack())
	if _, ok := nodes["node:a.md"]; !ok {
		t.Errorf("the completed group was not recorded before the failure: %v", nodes)
	}
	if _, ok := nodes["node:b.md"]; ok {
		t.Errorf("the failed group must not be recorded: %v", nodes)
	}
}

// A node whose content spans several transactions is described correctly only once
// all of them have landed, so its record waits for the group's LAST transaction —
// a partial record would claim a body the mirror does not have.
func TestTransportWritesBackOnlyWhenAGroupIsComplete(t *testing.T) {
	be := fake.New()
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		{
			Txn: fakeTxn(be, "node:big.md"), Group: "node:big.md",
			Produces: []publish.SymbolicID{"node:big.md"}, NodeStamp: publish.NodeStamp{Hash: "hBig"},
		},
		{
			Txn: fakeTxn(be, "node:big.md"), Group: "node:big.md",
			NodeStamp: publish.NodeStamp{Hash: "hBig"},
		},
	}}

	if _, err := New(be).Run(context.Background(), dag, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(be.WrittenBack()); got != 1 {
		t.Errorf("a two-transaction group wrote back %d times, want 1 (after its last txn)", got)
	}
}

// A group that fails on its continuation records nothing: the node's body is
// half-written, and claiming the full hash would make the next run hash-skip it.
func TestTransportRecordsNothingForAHalfWrittenGroup(t *testing.T) {
	inner := fake.New()
	be := &failingBackend{Backend: inner, failOn: 2}
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		{
			Txn: fakeTxn(inner, "node:big.md"), Group: "node:big.md",
			Produces: []publish.SymbolicID{"node:big.md"}, NodeStamp: publish.NodeStamp{Hash: "hBig"},
		},
		{
			Txn: fakeTxn(inner, "node:big.md"), Group: "node:big.md",
			NodeStamp: publish.NodeStamp{Hash: "hBig"},
		},
	}}

	if _, err := New(be).Run(context.Background(), dag, nil); err == nil {
		t.Fatal("Run should have failed")
	}
	if got := len(inner.WrittenBack()); got != 0 {
		t.Errorf("half-written group recorded %d write-back(s), want 0", got)
	}
}
