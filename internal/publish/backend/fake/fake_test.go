package fake_test

import (
	"context"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/backend/fake"
)

// The fake satisfies the Backend umbrella (hence all four roles) at compile time
// — the property that makes it a drop-in harness for later stage tests.
var _ backend.Backend = (*fake.Backend)(nil)

// Tokenize emits one unit per block, Cost=1, propagating the document's Group and
// each block's Refs/Anchors verbatim.
func TestTokenizeOneUnitPerBlock(t *testing.T) {
	b := fake.New()
	doc := publish.Document{
		Group: "node:page.md",
		Blocks: []publish.Block{
			{Content: "intro", Refs: []publish.SymbolicID{"node:other.md"}},
			{Content: "body", Anchors: []publish.AnchorName{"root-kek"}},
		},
	}
	units := b.Tokenize(doc)
	if len(units) != 2 {
		t.Fatalf("got %d units, want one per block (2)", len(units))
	}
	for i, u := range units {
		if u.Group != "node:page.md" {
			t.Errorf("unit %d Group = %q, want the document group", i, u.Group)
		}
		if u.Cost != 1 {
			t.Errorf("unit %d Cost = %v, want 1", i, u.Cost)
		}
	}
	if got := units[0].Refs; len(got) != 1 || got[0] != "node:other.md" {
		t.Errorf("unit 0 Refs = %v, want [node:other.md] preserved", got)
	}
	if got := units[1].Anchors; len(got) != 1 || got[0] != "root-kek" {
		t.Errorf("unit 1 Anchors = %v, want [root-kek] preserved", got)
	}
	if units[0].Payload != "intro" {
		t.Errorf("unit 0 Payload = %v, want the block content passed through", units[0].Payload)
	}
}

// A bounded Bin accepts up to MaxCount units then refuses, and does not mutate on
// a refused Add. Build seals whatever was accepted.
func TestBinMaxCountRefusesBeyondCeiling(t *testing.T) {
	b := fake.New(fake.WithMaxCount(2))
	bin := b.NewBin()
	u := publish.AtomicUnit{Group: "g", Cost: 1}
	if !bin.Add(u) || !bin.Add(u) {
		t.Fatal("first two Adds should fit under MaxCount=2")
	}
	if bin.Add(u) {
		t.Fatal("third Add should be refused at MaxCount=2")
	}
	// A refused Add must not consume capacity: still refuses, deterministically.
	if bin.Add(u) {
		t.Fatal("Add after a refusal should still be refused (no mutation on refusal)")
	}
	txn := bin.Build()
	if txn == nil {
		t.Fatal("Build should seal a non-nil transaction")
	}
}

// An unbounded ConstraintModel (MaxCount=0) never refuses — the harness dial that
// removes all packing pressure.
func TestBinUnboundedNeverRefuses(t *testing.T) {
	b := fake.New() // default: unbounded
	bin := b.NewBin()
	for i := 0; i < 1000; i++ {
		if !bin.Add(publish.AtomicUnit{Group: "g", Cost: 1}) {
			t.Fatalf("unbounded bin refused at i=%d", i)
		}
	}
}

// Execute mints a synthetic BackendID for the transaction's node (its Group) and
// for each hosted anchor, records the transaction, and returns those table
// updates.
func TestExecuteMintsIDsAndRecords(t *testing.T) {
	b := fake.New()
	bin := b.NewBin()
	bin.Add(publish.AtomicUnit{Group: "node:page.md", Anchors: []publish.AnchorName{"root-kek"}})
	txn := bin.Build()

	// The fake ignores the Resolver (readiness gating is the transport's job), so
	// nil is a fine stand-in here.
	res, err := b.Execute(context.Background(), txn, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	nodeID, ok := res.Nodes["node:page.md"]
	if !ok || nodeID == "" {
		t.Fatalf("ExecResult.Nodes missing a minted id for the group, got %v", res.Nodes)
	}
	anchorID, ok := res.Anchors["root-kek"]
	if !ok || anchorID == "" {
		t.Fatalf("ExecResult.Anchors missing a minted id for root-kek, got %v", res.Anchors)
	}
	if recs := b.Executed(); len(recs) != 1 {
		t.Fatalf("Execute should record exactly one transaction, got %d", len(recs))
	}
}

// Minted ids are unique across transactions — two creates never collide.
func TestExecuteMintsUniqueIDs(t *testing.T) {
	b := fake.New()
	mint := func(group publish.GroupKey) publish.BackendID {
		bin := b.NewBin()
		bin.Add(publish.AtomicUnit{Group: group})
		res, err := b.Execute(context.Background(), bin.Build(), nil)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return res.Nodes[publish.SymbolicID(group)]
	}
	if a, b2 := mint("node:a.md"), mint("node:b.md"); a == b2 {
		t.Fatalf("two creates minted the same id %q", a)
	}
}

// Scan returns the canned CurrentState the harness was seeded with, queryable
// through the four consumer methods.
func TestScanReturnsCannedState(t *testing.T) {
	cs := publish.NewCurrentState(
		map[publish.SymbolicID]publish.BackendID{"node:a.md": "be-a"},
		map[publish.SymbolicID]publish.Hash{"node:a.md": "h1"},
		map[publish.AnchorName]publish.BackendID{"root-kek": "be-anchor"},
	)
	b := fake.New(fake.WithScan(cs))

	got, err := b.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if id, ok := got.NodeID("node:a.md"); !ok || id != "be-a" {
		t.Errorf("NodeID = (%q,%v), want (be-a,true)", id, ok)
	}
	if h, ok := got.ContentHash("node:a.md"); !ok || h != "h1" {
		t.Errorf("ContentHash = (%q,%v), want (h1,true)", h, ok)
	}
	if id, ok := got.AnchorID("root-kek"); !ok || id != "be-anchor" {
		t.Errorf("AnchorID = (%q,%v), want (be-anchor,true)", id, ok)
	}
	var nodes []publish.SymbolicID
	for n := range got.Nodes() {
		nodes = append(nodes, n)
	}
	if len(nodes) != 1 || nodes[0] != "node:a.md" {
		t.Errorf("Nodes() = %v, want [node:a.md]", nodes)
	}
}

// A default fake with no seeded scan returns an empty (non-nil) CurrentState.
func TestScanDefaultsToEmpty(t *testing.T) {
	got, err := fake.New().Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got == nil {
		t.Fatal("Scan should return a non-nil empty CurrentState by default")
	}
	if _, ok := got.NodeID("node:anything"); ok {
		t.Error("default scan should have no nodes")
	}
}
