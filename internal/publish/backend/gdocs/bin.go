package gdocs

import (
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// NewBin opens a request accumulator capped at maxRequests.
func (b *Backend) NewBin() backend.Bin { return &bin{} }

// bin accumulates units until their combined request cost exceeds maxRequests.
//
// FIGHT (the Group ceiling is the wrong axis). A batchUpdate is document-wide: one
// call can carry requests for EVERY tab, and with the binding quota at 60 writes
// per minute per user, fusing across nodes is exactly what this backend wants. But
// GroupKey is an ANTI-BUNDLING key — units of different Groups may never share a
// transaction — and Group is the node. So the seam forces one batchUpdate per
// node, which is the opposite of the optimum here. Notion needed per-page grouping
// because its write endpoint IS per-page; Docs' is per-document.
type bin struct {
	group    publish.GroupKey
	hasGroup bool
	cost     int
	units    []unit
}

type unit struct {
	payload publish.BackendBlock
	refs    []publish.SymbolicID
	anchors []publish.AnchorName
}

func (bn *bin) Add(u publish.AtomicUnit) bool {
	c, _ := u.Cost.(int)
	if len(bn.units) > 0 && bn.cost+c > maxRequests {
		return false
	}
	if !bn.hasGroup {
		bn.group, bn.hasGroup = u.Group, true
	}
	bn.cost += c
	bn.units = append(bn.units, unit{payload: u.Payload, refs: u.Refs, anchors: u.Anchors})
	return true
}

func (bn *bin) Build() publish.Transaction {
	return &Transaction{group: bn.group, units: bn.units}
}

// Transaction is one sealed batchUpdate.
type Transaction struct {
	group publish.GroupKey
	units []unit
}
