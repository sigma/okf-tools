package fs

import (
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// NewBin opens a fresh filesystem accumulator. It is UNBOUNDED: unlike Notion's
// ≤100-block bin, it advertises no capacity ceiling at all, so the packing
// strategy opens exactly one bin per Group and seals it once.
func (b *Backend) NewBin() backend.Bin { return &bin{} }

// bin is the filesystem capacity accumulator — the deliberate inverse of Notion's.
// It is unbounded and non-fusing:
//
//   - Add NEVER refuses a unit (there is no ceiling and no delete-standalone rule
//     to defend — a filesystem write cannot overflow), so every unit of a Group
//     accumulates into one transaction;
//   - Build does NOT collapse anything: it seals the units in order, and the
//     Executor then performs one filesystem write per unit (1 unit = 1 write).
//
// Where Notion's Build fuses a create + its properties + its first content chunk
// into a single POST /pages, this Build fuses nothing — the whole "no fusion"
// story is that Execute walks the units and writes each on its own. Because Add
// never refuses, the optimizer's next-fit strategy produces one PackedTxn per
// group; the create/props/content ordering the optimizer guarantees is preserved
// so the exported files land in document order.
type bin struct {
	group    publish.GroupKey
	hasGroup bool
	units    []unit
}

// unit is one accumulated AtomicUnit reduced to what Execute needs: its opaque
// payload, the Refs the transport gated on (a create's parent; a content unit's
// outbound links and write-target), and the anchors it hosts.
type unit struct {
	payload publish.BackendBlock
	refs    []publish.SymbolicID
	anchors []publish.AnchorName
}

// Add accumulates a unit and always returns true — the unbounded, never-refuses
// contract. It records the bin's Group from the first unit (the optimizer only
// ever hands it units of one Group).
func (bn *bin) Add(u publish.AtomicUnit) bool {
	if !bn.hasGroup {
		bn.group = u.Group
		bn.hasGroup = true
	}
	bn.units = append(bn.units, unit{payload: u.Payload, refs: u.Refs, anchors: u.Anchors})
	return true
}

// Build seals the accumulated units into one opaque filesystem transaction,
// preserving their order. No fusion happens here; the writes fan out at Execute.
// The bin must not be used after Build.
func (bn *bin) Build() publish.Transaction {
	return &Transaction{group: bn.group, units: bn.units}
}

// Transaction is the filesystem backend's opaque sealed API call, produced by
// Bin.Build. It is opaque to the pipeline; the filesystem Executor turns it into a
// set of writes under the node's export directory — the node.json existence
// marker for a create, props.json for properties, one section file per content
// unit, or a subtree removal for a delete. group is the node's affinity key, which
// (as a "node:<path>" symbolic id) also names the export directory.
type Transaction struct {
	group publish.GroupKey
	units []unit
}
