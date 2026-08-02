package publish

import (
	"iter"
	"maps"
	"slices"
)

// CurrentState is the backend-neutral current-state snapshot a Scanner produces.
// It (a) seeds the resolution table with already-existing nodes and anchors and
// (b) drives change detection.
//
// The reconstruction is behind the seam; the product is neutral. How a backend
// rebuilds ContentHash and AnchorID (hash reconstruction over HTTP, the #146
// subtree trick, pagination) is invisible here; once produced, a CurrentState is
// plain neutral data queried through four methods. #163 fixes only these four
// consumer-side queries — the contract Generation compiles against; the
// producer-side reconstruction, and any extra fields it needs, are #167's.
type CurrentState struct {
	nodeIDs   map[SymbolicID]BackendID
	hashes    map[SymbolicID]Hash
	anchorIDs map[AnchorName]BackendID
	// order is the deterministic iteration order for Nodes(), derived once at
	// construction from the sorted node ids.
	order []SymbolicID
}

// NewCurrentState builds a neutral CurrentState from a backend's reconstructed
// tables. It defensively copies the maps so a backend may reuse its own, and
// precomputes a deterministic node-iteration order. Passing nil for any table is
// treated as empty. #167 will extend this constructor as it adds fields.
func NewCurrentState(nodeIDs map[SymbolicID]BackendID, hashes map[SymbolicID]Hash, anchorIDs map[AnchorName]BackendID) *CurrentState {
	cs := &CurrentState{
		nodeIDs:   maps.Clone(nodeIDs),
		hashes:    maps.Clone(hashes),
		anchorIDs: maps.Clone(anchorIDs),
	}
	if cs.nodeIDs == nil {
		cs.nodeIDs = map[SymbolicID]BackendID{}
	}
	if cs.hashes == nil {
		cs.hashes = map[SymbolicID]Hash{}
	}
	if cs.anchorIDs == nil {
		cs.anchorIDs = map[AnchorName]BackendID{}
	}
	cs.order = slices.Sorted(maps.Keys(cs.nodeIDs))
	return cs
}

// NodeID returns the backend id of an already-existing node, seeding the
// resolution table (a Ref to it resolves with no edge) and, by its absence,
// signalling a node that must be created.
func (cs *CurrentState) NodeID(id SymbolicID) (BackendID, bool) {
	b, ok := cs.nodeIDs[id]
	return b, ok
}

// ContentHash returns the last-known content hash of a node: the "changed /
// drifted?" arm of the diff. A scanned hash that matches the expected hash is
// hash-skipped.
func (cs *CurrentState) ContentHash(id SymbolicID) (Hash, bool) {
	h, ok := cs.hashes[id]
	return h, ok
}

// AnchorID returns the backend id of an already-hosted anchor, so a page linking
// into an unchanged glossary resolves the anchor straight from the scan seed
// with no forced rewrite.
func (cs *CurrentState) AnchorID(name AnchorName) (BackendID, bool) {
	b, ok := cs.anchorIDs[name]
	return b, ok
}

// Nodes iterates every node present in the snapshot, in a deterministic order,
// driving orphan / vanished detection (a scanned node with no source → DeleteNode).
func (cs *CurrentState) Nodes() iter.Seq[SymbolicID] {
	return slices.Values(cs.order)
}
