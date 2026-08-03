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
	nodeIDs    map[SymbolicID]BackendID
	hashes     map[SymbolicID]Hash
	propHashes map[SymbolicID]Hash
	anchorIDs  map[AnchorName]BackendID
	// order is the deterministic iteration order for Nodes(), derived once at
	// construction from the sorted node ids.
	order []SymbolicID
}

// NewCurrentState builds a neutral CurrentState from a backend that reconstructs no
// property hashes (the common seam: the content hash and anchors alone). It is a
// thin wrapper over NewCurrentStateWithProps with an empty property-hash table.
func NewCurrentState(nodeIDs map[SymbolicID]BackendID, hashes map[SymbolicID]Hash, anchorIDs map[AnchorName]BackendID) *CurrentState {
	return NewCurrentStateWithProps(nodeIDs, hashes, nil, anchorIDs)
}

// NewCurrentStateWithProps builds a neutral CurrentState including the per-node
// property-hash table the two-hash split needs (#110 phase 2): a scanner that reads
// back a stored property hash supplies it here so SetProperties can hash-skip
// independently of SetContent. It defensively copies the maps and precomputes a
// deterministic node-iteration order. Passing nil for any table is treated as empty.
func NewCurrentStateWithProps(nodeIDs map[SymbolicID]BackendID, hashes, propHashes map[SymbolicID]Hash, anchorIDs map[AnchorName]BackendID) *CurrentState {
	cs := &CurrentState{
		nodeIDs:    maps.Clone(nodeIDs),
		hashes:     maps.Clone(hashes),
		propHashes: maps.Clone(propHashes),
		anchorIDs:  maps.Clone(anchorIDs),
	}
	if cs.nodeIDs == nil {
		cs.nodeIDs = map[SymbolicID]BackendID{}
	}
	if cs.hashes == nil {
		cs.hashes = map[SymbolicID]Hash{}
	}
	if cs.propHashes == nil {
		cs.propHashes = map[SymbolicID]Hash{}
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

// PropertyHash returns the last-known property hash of a node: the "properties
// drifted?" arm of the diff, gating SetProperties independently of ContentHash. A
// scanner that reconstructs no property hash leaves this absent, which the gate
// reads leniently (no forced property rewrite).
func (cs *CurrentState) PropertyHash(id SymbolicID) (Hash, bool) {
	h, ok := cs.propHashes[id]
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
