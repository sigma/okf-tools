package gdocs

import (
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// NewBin opens a fresh request accumulator.
func (b *Backend) NewBin() backend.Bin { return &bin{} }

// bin accumulates one transaction's worth of units, bounded by request count.
//
// The Group partition the optimizer applies means one bin per NODE, so a
// document-wide batchUpdate is never assembled. That was measured and accepted:
// the extra calls come from cycle-breaking (a link needs its target's minted
// tabId), not from the partition, and Group also carries in-order execution,
// incremental write-back and cycle-break granularity (#156).
type bin struct {
	group    publish.GroupKey
	hasGroup bool
	cost     int
	units    []unit
}

// unit is an accumulated AtomicUnit reduced to what Execute needs.
type unit struct {
	payload publish.BackendBlock
	refs    []publish.SymbolicID
	anchors []publish.AnchorName
}

func (bn *bin) Add(u publish.AtomicUnit) bool {
	c, _ := u.Cost.(int)
	if len(bn.units) > 0 && bn.cost+c > maxRequestsPerBatch {
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

// Transaction is one sealed batchUpdate's worth of work.
type Transaction struct {
	group publish.GroupKey
	units []unit
}
