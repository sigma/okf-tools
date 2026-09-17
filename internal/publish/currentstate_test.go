package publish_test

import (
	"slices"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
)

// Nodes() iterates every node deterministically (sorted), regardless of the map's
// randomized range order — so change-detection over a scan is reproducible.
func TestCurrentStateNodesDeterministic(t *testing.T) {
	cs := publish.NewCurrentState(
		map[publish.SymbolicID]publish.BackendID{
			"node:c.md": "c", "node:a.md": "a", "node:b.md": "b",
		}, nil, nil)

	var first []publish.SymbolicID
	for n := range cs.Nodes() {
		first = append(first, n)
	}
	want := []publish.SymbolicID{"node:a.md", "node:b.md", "node:c.md"}
	if !slices.Equal(first, want) {
		t.Fatalf("Nodes() = %v, want sorted %v", first, want)
	}
	// Stable across calls.
	var second []publish.SymbolicID
	for n := range cs.Nodes() {
		second = append(second, n)
	}
	if !slices.Equal(first, second) {
		t.Fatalf("Nodes() order not stable: %v then %v", first, second)
	}
}

// NewCurrentState copies its input maps: mutating the caller's map afterward must
// not leak into the snapshot.
func TestCurrentStateDefensiveCopy(t *testing.T) {
	nodes := map[publish.SymbolicID]publish.BackendID{"node:a.md": "a"}
	cs := publish.NewCurrentState(nodes, nil, nil)
	nodes["node:a.md"] = "tampered"
	delete(nodes, "node:a.md")

	if id, ok := cs.NodeID("node:a.md"); !ok || id != "a" {
		t.Fatalf("NodeID = (%q,%v), want (a,true) — snapshot must not alias caller's map", id, ok)
	}
}

// Missing keys report absence, not a zero-value hit — the "must create / has
// drifted" arms of the diff depend on the bool.
func TestCurrentStateMissingKeys(t *testing.T) {
	cs := publish.NewCurrentState(nil, nil, nil)
	if _, ok := cs.NodeID("node:x"); ok {
		t.Error("NodeID on empty state should report absence")
	}
	if _, ok := cs.ContentHash("node:x"); ok {
		t.Error("ContentHash on empty state should report absence")
	}
	if _, ok := cs.AnchorID("a"); ok {
		t.Error("AnchorID on empty state should report absence")
	}
	if n := slices.Collect(cs.Nodes()); len(n) != 0 {
		t.Errorf("Nodes() on empty state = %v, want none", n)
	}
}

// Owner reports where the scan found a node RECORDED: "" for a node that is its
// own row, the recording ancestor's symbolic id for a subpage, and absent when the
// scanner supplies no such fact at all — three answers, and the diff needs to tell
// the first from the third (sigma/okf-tools#209).
func TestCurrentStateOwner(t *testing.T) {
	cs := publish.NewCurrentStateWithOwners(
		map[publish.SymbolicID]publish.BackendID{"node:dir/README.md": "r", "node:dir/a.md": "a"},
		nil, nil, nil,
		map[publish.SymbolicID]publish.SymbolicID{"node:dir/README.md": "", "node:dir/a.md": "node:dir/README.md"},
	)
	if o, ok := cs.Owner("node:dir/README.md"); !ok || o != "" {
		t.Errorf("Owner(README) = (%q,%v), want (\"\",true): a row records itself", o, ok)
	}
	if o, ok := cs.Owner("node:dir/a.md"); !ok || o != "node:dir/README.md" {
		t.Errorf("Owner(a.md) = (%q,%v), want its recording row", o, ok)
	}
	if _, ok := cs.Owner("node:elsewhere.md"); ok {
		t.Error("Owner of a node the scan never described should report absence")
	}

	plain := publish.NewCurrentState(map[publish.SymbolicID]publish.BackendID{"node:a.md": "a"}, nil, nil)
	if _, ok := plain.Owner("node:a.md"); ok {
		t.Error("a snapshot built without owners must report absence, not a self-owned row")
	}
}
