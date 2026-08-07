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
	// asserts records that one of the accepted content units is its node's first —
	// which makes this bin the transaction that asserts the node's whole content
	// rather than one that continues it (#130).
	assertsContent bool
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
		// Thread the unit's hosted anchors onto the block so Build carries them and
		// the Executor can map anchor-name → the block's real Notion id.
		p.anchors = u.Anchors
		bn.assertsContent = bn.assertsContent || p.assertsContent
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
	t := &Transaction{Group: bn.group, Children: bn.children, AssertsContent: bn.assertsContent}
	if bn.create != nil {
		t.Create = true
		t.Node = bn.create.node
		// The Executor resolves the parent to the real parent page id at execute
		// time (empty parent → a top-level row under the data source).
		t.Parent = bn.create.parent
	} else if bn.props != nil {
		// No create in this bin: the properties unit is the one that knows the parent,
		// so a standalone property update still knows whether its target is a
		// page-parented child_page or a data-source row — the #104 rule the update path
		// used to be blind to (#128). A co-binned create is authoritative instead (both
		// units are stamped from the same neutral op, so they agree).
		t.Parent = bn.props.parent
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
	// Parent is the symbolic id of the node's parent: empty for a top-level node
	// (a data-source row), else the cluster index page it nests under as a
	// child_page. It is set on any transaction that writes the node's properties —
	// a create OR a standalone/fused property update — because it selects the
	// parent KIND, and the parent kind decides which properties may be written at
	// all (a child_page has no data-source columns, #104/#128). On a create the
	// Executor additionally resolves it to the real parent page id for POST /pages;
	// on an update it is only read, never resolved. A content-only append carries
	// none, since it writes no properties.
	Parent publish.SymbolicID
	// Create marks a page-create (POST /pages) fusing Props and the first Children.
	Create bool
	// Delete marks an archive of Node.
	Delete bool
	// AssertsContent marks this transaction as the one asserting the node's COMPLETE
	// expected content: its Children are the node's whole body, not an increment.
	// The update path therefore clears the page's existing children before
	// appending them, so a re-publish replaces the old body instead of doubling it
	// (sigma/okf-tools#130). It is set on the transaction holding the node's first
	// content block; the follow-on overflow chunks of that same assertion carry it
	// false and merely append. A create carries it too but has nothing to clear —
	// POST /pages makes the page and its first children in one call — so the create
	// path ignores it.
	AssertsContent bool
	// Props are the neutral properties to set (fused into a create, or a
	// standalone property update), nil if none.
	Props map[string]any
	// Children are the ordered Notion child blocks this transaction appends.
	Children []childBlock
}
