package graph

import "github.com/sigma/okf-tools/internal/publish"

// OpKind is one of the four semantic operations that make up the op-DAG
// (sigma/ideas#162). Every op names exactly one node; ops that produce something
// mint a symbolic id, ops that consume embed Refs.
type OpKind int

const (
	// CreateNode establishes a node's existence (empty) under a parent. Splitting
	// existence from content is the acyclicity mechanism: content-refs-node edges
	// target the CreateNode, never the SetContent, so mutual links never cycle.
	CreateNode OpKind = iota
	// SetProperties asserts a node's semantic properties (title, type, frontmatter
	// columns) — backend-neutral values, never Notion column shapes.
	SetProperties
	// SetContent writes a node's backend-neutral document tree; it declares named
	// anchors and may embed first-class Ref nodes.
	SetContent
	// DeleteNode reconciles an orphan: a single op on the vanished subtree root,
	// which the backend archives as a whole.
	DeleteNode
)

func (k OpKind) String() string {
	switch k {
	case CreateNode:
		return "CreateNode"
	case SetProperties:
		return "SetProperties"
	case SetContent:
		return "SetContent"
	case DeleteNode:
		return "DeleteNode"
	default:
		return "OpKind(?)"
	}
}

// Op is a node of the operation DAG: one semantic, backend-neutral operation.
// The fields that carry a payload are populated by kind — Parent for CreateNode,
// Props for SetProperties, Doc/Refs/Anchors for SetContent — and are otherwise
// zero.
type Op struct {
	// Kind selects which of the four operations this is.
	Kind OpKind
	// Node is the symbolic id of the node this op acts on or produces:
	// "node:<bundle-rel-path>" (e.g. "node:docs/adr/0002.md").
	Node publish.SymbolicID
	// Parent is the symbolic id of the node this node is created / lives under, or
	// "" for a node at the top of its area database (today's null-parent / area-DB
	// row). On a CreateNode it is the parent-before-child ref: a parent already in
	// the scan seeds without an edge; a parent created this run gets an edge. It is
	// also stamped on a touched node's SetProperties / SetContent ops so publish-
	// time write-back can route a subpage's provenance into its parent row's
	// subtree map even when the node already exists (no CreateNode this run).
	Parent publish.SymbolicID
	// Hash is the node's expected content hash — the value change detection
	// computed for this run. It is stamped on a touched node's ops so it can be
	// threaded to the transport and written back into the node's `hash` derived
	// column, making the next ScanStored hash-skip it. Zero for a DeleteNode.
	//
	// Every op a touched node emits carries the node's FULL expected hash state
	// (both Hash and PropHash), not just the arm it represents, so write-back can
	// stamp both columns to their expected values from whichever arm survived the
	// gate — a content-only re-publish still re-asserts the (unchanged) property
	// hash, and vice-versa, without a partial-column read-modify-write.
	Hash publish.Hash
	// PropHash is the node's expected property hash — a fingerprint over its
	// semantic properties (title, type, frontmatter; graph.PropertyHash). It gates
	// SetProperties independently of Hash gating SetContent, so a title-only edit
	// re-asserts properties without rewriting the body and a body-only edit rewrites
	// content without touching properties. Zero for a DeleteNode.
	PropHash publish.Hash
	// Title is the node's display title, stamped on a touched node's ops so
	// publish-time write-back can record it in a subpage's subtree-map entry — the
	// key ScanRecompute matches a live page back to its subpath by. Empty for a
	// DeleteNode.
	Title string
	// Props are the semantic properties SetProperties asserts. Set only for
	// SetProperties.
	Props map[string]any
	// Doc is the backend-neutral document SetContent writes: its Group is the
	// target node, its blocks carry first-class Ref inlines and declared anchors.
	// Set only for SetContent.
	Doc *publish.Document
	// Refs are the symbolic ids this op embeds and the transport must resolve
	// before executing it — "node:<path>" and "anchor:<name>". Aggregated from
	// Doc's blocks. Set only for SetContent.
	Refs []publish.SymbolicID
	// Anchors are the named anchors this op declares (a glossary's SetContent):
	// this op is their producer. Set only for SetContent on the anchor host.
	Anchors []publish.AnchorName
}

// EdgeCause is one of the three — and only three — reasons a dependency edge
// exists (sigma/ideas#162, decision 5).
type EdgeCause int

const (
	// ParentBeforeChild: CreateNode(child) depends on CreateNode(parent) — the
	// child needs the parent's real id to be parented under it.
	ParentBeforeChild EdgeCause = iota
	// ContentRefsNode: SetContent(A) embedding Ref{node:B} depends on
	// CreateNode(B) — B's existence, not its content, which is why mutual links
	// resolve as two edges into two creates with no cycle.
	ContentRefsNode
	// ContentRefsAnchor: SetContent(A) embedding Ref{anchor:X} depends on the
	// SetContent that hosts X (the glossary body) — an anchor's real block id only
	// exists once that body is written (the #160 glossary ordering, as just an edge).
	ContentRefsAnchor
)

func (c EdgeCause) String() string {
	switch c {
	case ParentBeforeChild:
		return "parent-before-child"
	case ContentRefsNode:
		return "content-refs-node"
	case ContentRefsAnchor:
		return "content-refs-anchor"
	default:
		return "EdgeCause(?)"
	}
}

// Edge is a "must complete before" dependency: From must complete before To.
// (For a content-refs edge, To is the consuming SetContent and From is the
// producing CreateNode / SetContent it waits on.)
type Edge struct {
	From  *Op
	To    *Op
	Cause EdgeCause
}

// Graph is the op-DAG Stage 1 emits: the semantic operations and the dependency
// edges between them. Nodes are Ops; edges mean "must complete before". An
// unchanged bundle assembles a near-edgeless graph — every Ref resolves from the
// scan seed.
type Graph struct {
	Ops   []*Op
	Edges []Edge
}
