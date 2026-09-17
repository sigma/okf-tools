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
// plain neutral data queried through a handful of methods. #163 fixed the first
// four consumer-side queries — the contract Generation compiles against — and #209
// added Owner; the producer-side reconstruction, and any extra fields it needs,
// are #167's.
type CurrentState struct {
	nodeIDs    map[SymbolicID]BackendID
	hashes     map[SymbolicID]Hash
	propHashes map[SymbolicID]Hash
	anchorIDs  map[AnchorName]BackendID
	// owners is where the scan found each node RECORDED: "" for a node that is its
	// own row, else the recording ancestor's symbolic id. A node absent here is one
	// the scanner said nothing about (see Owner).
	owners map[SymbolicID]SymbolicID
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
	return NewCurrentStateWithOwners(nodeIDs, hashes, propHashes, anchorIDs, nil)
}

// NewCurrentStateWithOwners builds a neutral CurrentState that also carries, per
// node, WHERE the destination records it (see Owner): a scanner whose destination
// keeps a node's record somewhere other than the node itself supplies it here so
// change detection can see a node whose recording ancestor changed
// (sigma/okf-tools#209). Passing nil leaves every node's owner unknown, which the
// diff reads as "no re-parent can be detected" — the behaviour of a scanner that
// predates the fact.
func NewCurrentStateWithOwners(nodeIDs map[SymbolicID]BackendID, hashes, propHashes map[SymbolicID]Hash, anchorIDs map[AnchorName]BackendID, owners map[SymbolicID]SymbolicID) *CurrentState {
	cs := &CurrentState{
		nodeIDs:    maps.Clone(nodeIDs),
		hashes:     maps.Clone(hashes),
		propHashes: maps.Clone(propHashes),
		anchorIDs:  maps.Clone(anchorIDs),
		owners:     maps.Clone(owners),
	}
	if cs.owners == nil {
		cs.owners = map[SymbolicID]SymbolicID{}
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

// Owner reports where the destination RECORDS a node, per the scan: "" when the
// node is its own row (it records itself), else the symbolic id of the ancestor
// row whose subtree map holds it — the stored counterpart of the Owner a source
// hierarchy stamps on every op. The bool is whether the scanner supplied the fact at
// all, which is distinct from the "" answer: a snapshot that knows nothing about
// owners must not read as "every node is a row", or every subpage would look
// re-parented.
//
// It is the "moved?" arm of the diff (sigma/okf-tools#209): a node whose stored owner
// differs from its expected one is not an update but a re-parent — the old page has
// to go and a new one be created under the new parent, or two pages end up claiming
// one path. It deliberately reports the OWNER (the recording row) and not the parent
// (where the page lives): only the former is stored, so a move that changes the
// parent but keeps the owning row — between two nested indexes under one row — is
// invisible to it.
func (cs *CurrentState) Owner(id SymbolicID) (SymbolicID, bool) {
	o, ok := cs.owners[id]
	return o, ok
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
