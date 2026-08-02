package notion

import (
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// NewBin opens a fresh Notion accumulator honoring this backend's block ceiling.
// The optimizer partitions units by Group before packing, so a bin only ever sees
// units of one node — but the bin defends the invariant anyway (it refuses to
// co-bin across Group).
func (b *Backend) NewBin() backend.Bin {
	return &bin{maxBlocks: b.maxBlocks}
}

// bin is the Notion capacity accumulator. It is O(1) per Add — it keeps running
// tallies (childCost, the create/props/delete presence flags), never re-scanning
// its accepted units — and it enforces Notion's two coupled limits together with
// its fusion rule:
//
//   - it holds units of a single Group (refusing any unit of a different Group);
//   - it accepts at most one create and one properties unit, plus content units up
//     to the ≤100 child-block ceiling (create/props cost zero children);
//   - a delete unit stands alone: it never co-bins with create/props/content, and
//     nothing co-bins into a delete bin.
//
// Build() then fuses whatever co-binned — a create + its properties + its first
// content chunk collapse into one transaction (Notion's POST /pages). A content
// unit that overflows the ceiling is refused, so the optimizer seals this bin and
// opens a fresh one: that overflow bin has no create, exposes the node's
// write-target Ref, and becomes a follow-on PATCH children (an edge back to the
// create). Refusing to co-bin is the whole of the "no fusion" story for a
// non-fusing backend — same code path.
type bin struct {
	maxBlocks int

	group    publish.GroupKey
	hasGroup bool

	create    *createBlock
	props     *propsBlock
	del       *deleteBlock
	children  []childBlock
	childCost int
}

// Add tries to add a unit, returning false without mutating the bin if the unit
// would violate the Group affinity, the block ceiling, or the delete-standalone
// rule.
func (bn *bin) Add(u publish.AtomicUnit) bool {
	// Group affinity: never co-bin across pages.
	if bn.hasGroup && u.Group != bn.group {
		return false
	}

	switch p := u.Payload.(type) {
	case createBlock:
		if bn.create != nil || bn.del != nil {
			return false
		}
		bn.claim(u.Group)
		bn.create = &p
		return true

	case propsBlock:
		if bn.props != nil || bn.del != nil {
			return false
		}
		bn.claim(u.Group)
		bn.props = &p
		return true

	case childBlock:
		if bn.del != nil {
			return false
		}
		cost := blockCost(u.Cost)
		if bn.childCost+cost > bn.maxBlocks {
			return false
		}
		bn.claim(u.Group)
		bn.children = append(bn.children, p)
		bn.childCost += cost
		return true

	case deleteBlock:
		// A delete stands alone: nothing may already be here, and nothing may join.
		if bn.hasGroup {
			return false
		}
		bn.claim(u.Group)
		bn.del = &p
		return true

	default:
		// A foreign / unknown payload: refuse rather than silently misfuse.
		return false
	}
}

// Build seals the accumulated units into one opaque Notion transaction, fusing a
// co-binned create + properties + first content chunk. The bin must not be used
// after Build.
func (bn *bin) Build() publish.Transaction {
	t := &Transaction{Group: bn.group, Children: bn.children}
	if bn.create != nil {
		t.Create = true
		t.Node = bn.create.node
	}
	if bn.props != nil {
		t.Props = bn.props.props
		if t.Node == "" {
			t.Node = bn.props.node
		}
	}
	if bn.del != nil {
		t.Delete = true
		t.Node = bn.del.node
	}
	return t
}

// claim records the bin's Group on first accepted unit.
func (bn *bin) claim(g publish.GroupKey) {
	if !bn.hasGroup {
		bn.group = g
		bn.hasGroup = true
	}
}

// blockCost reads a unit's Cost as a Notion child-block count, defaulting to one
// block for a unit whose Cost is not an int (defensive; the tokenizer always sets
// an int).
func blockCost(c publish.Cost) int {
	if n, ok := c.(int); ok {
		return n
	}
	return 1
}

// Transaction is the Notion backend's opaque sealed API call, produced by
// Bin.Build. It is opaque to the pipeline; the Notion Executor (#42) turns it into
// the wire call: with Create set it is a POST /pages carrying Props and up to the
// first 100 Children (the fusion); without Create it is a PATCH /blocks/{Node}/
// children appending Children; with Delete it archives Node. Node is a symbolic id
// the transport resolves at execute time.
type Transaction struct {
	// Group is the target node's affinity key, shared by every unit fused here.
	Group publish.GroupKey
	// Node is the symbolic id of the page this transaction creates, appends to, or
	// archives.
	Node publish.SymbolicID
	// Create marks a page-create (POST /pages) fusing Props and the first Children.
	Create bool
	// Delete marks an archive of Node.
	Delete bool
	// Props are the neutral properties to set (fused into a create, or a
	// standalone property update), nil if none.
	Props map[string]any
	// Children are the ordered Notion child blocks this transaction appends.
	Children []childBlock
}
