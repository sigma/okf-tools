package publish

// This file defines the backend-neutral currency that flows between the three
// okfpub pipeline stages. The pipeline speaks only this vocabulary; every
// backend specific (Notion's block model, its two coupled API limits, its HTTP
// transport) stays behind the role interfaces in the backend subpackage.
//
// The symbolic-id scheme (SymbolicID, AnchorName, and their constructors and
// readers) lives in ref.go.
//
// See sigma/ideas#172 for the ratified spec (decisions #162 and #163).

// BackendID is an opaque real backend identifier (a Notion page/block id, a
// filesystem path, …). It is neutral data to the pipeline: produced by the
// backend, threaded by the transport, never interpreted.
type BackendID string

// Hash is a content hash used by change detection: a node whose scanned hash
// matches its expected hash is hash-skipped (no SetContent). Its concrete
// derivation is the scan producer's concern (#167); to the pipeline it is an
// opaque neutral value.
type Hash string

// --- Packing dimensions (the two exposed by an AtomicUnit) ------------------

// GroupKey is the affinity / anti-bundling key: two AtomicUnits with different
// GroupKey may not share a transaction. It maps to the target node's identity
// (Notion: the page; a filesystem backend: the directory). Beyond packing, the
// transport keys per-target pacing off it, so it is transparent to the pipeline
// (partitioning and pacing read it directly).
type GroupKey string

// Cost is an abstract, backend-defined measure of what an AtomicUnit counts
// against a ConstraintModel's limits — deliberately not "bytes" or "blocks".
// The field is exposed as a packing dimension (packing forwards it to a Bin),
// but its value is opaque to the pipeline: only the backend's Bin knows how to
// accumulate it and compare it against its ceilings, so all capacity arithmetic
// stays behind the seam (a scalar ceiling would leak the assumption that cost is
// a single additive quantity — Notion has two coupled limits).
type Cost any

// --- The atomic unit --------------------------------------------------------

// BackendBlock is a backend's own atomic payload (a Notion block, a file
// section, …). It is fully opaque to the pipeline: the pipeline packs
// BackendBlocks without ever looking inside them, which is what lets one generic
// packing loop serve any backend.
type BackendBlock any

// AtomicUnit is the unit of currency produced by a Tokenizer and packed by
// optimization. It is opaque to the pipeline save for its two packing
// dimensions, Cost and Group; the pipeline never reads Payload. All four op
// types of #162 flow through this one shape (SetContent tokenizes into many
// units; CreateNode/SetProperties/DeleteNode each become one), so backend
// specific fusion can live inside a Bin.
type AtomicUnit struct {
	// Payload is the backend's own block; opaque to the pipeline.
	Payload BackendBlock
	// Cost is what this unit counts against the ConstraintModel's limits.
	Cost Cost
	// Group is the affinity key: what this unit may NOT be bundled across.
	Group GroupKey
	// Refs are the unresolved symbolic ids the transport must gate on before
	// this unit is ready to execute (#162). Ordering metadata, not rewriting.
	Refs []SymbolicID
	// Anchors are the named anchors this unit hosts, contributing to the anchor
	// map (a resolved anchor's backend id is this unit's node id).
	Anchors []AnchorName
}

// --- The non-content op seam (tokenizer input for the other three ops) -------

// NonContentOpKind selects which of the three non-content ops a NonContentOp
// describes. SetContent is not here — it flows through Tokenize as a Document;
// these are the ops that carry no block-granular content but still become one
// backend AtomicUnit each.
type NonContentOpKind int

const (
	// CreateOp establishes a node's existence: the backend mints its create block
	// (Notion: the shell of a POST /pages) so a Bin can fuse it with the node's
	// properties and first content chunk.
	CreateOp NonContentOpKind = iota
	// PropertiesOp asserts a node's semantic properties: the backend turns the
	// neutral Props map into its own property payload (Notion: page properties).
	PropertiesOp
	// DeleteOp archives an orphan node: the backend mints its delete/archive block.
	DeleteOp
)

// NonContentOp is the neutral descriptor the optimizer hands a Tokenizer so the
// backend can mint the single AtomicUnit for a CreateNode / SetProperties /
// DeleteNode op — the counterpart to how SetContent flows through Tokenize. It
// exists so that all four op types acquire their backend Payload and Cost behind
// the seam (letting a Notion Bin fuse create + properties + first content into
// one POST /pages), while the optimizer stays backend-agnostic: it never
// constructs a backend payload, and it — not the backend — stamps the unit's
// neutral Group, write-target/parent Refs, and graph provenance.
//
// It is deliberately narrower than a graph.Op: the backend needs only what shapes
// its payload (the kind, and Props for a PropertiesOp). Parent, edges, and
// symbolic-id bookkeeping remain the optimizer's concern.
type NonContentOp struct {
	// Kind selects create / properties / delete.
	Kind NonContentOpKind
	// Node is the symbolic id of the node this op acts on. The backend may fold it
	// into its payload as the transaction's write-target (e.g. the Notion page a
	// create/append/archive addresses); the transport still resolves it to a real
	// backend id at execute time.
	Node SymbolicID
	// Props are the neutral semantic properties, set only for a PropertiesOp; the
	// backend reshapes them into its own property payload.
	Props map[string]any
}

// --- The transaction and its result -----------------------------------------

// Transaction is one sealed, executable backend API call, produced by
// Bin.Build(). It is opaque to the pipeline: the thing that knows what fits (the
// Bin) is the thing that knows how to assemble, so backend-specific fusion lives
// behind Build(). The pipeline only holds Transactions in the wavefront and
// hands them to an Executor.
type Transaction any

// ExecResult carries the resolution-table updates a transaction produced: new
// symbolic-id → backend-id pairs for the nodes it created, plus anchor-name →
// backend-id for the anchors it hosted. The transport merges these back into the
// table so downstream Refs resolve.
type ExecResult struct {
	// Nodes maps each symbolic id this transaction produced to its new backend id.
	Nodes map[SymbolicID]BackendID
	// Anchors maps each anchor this transaction hosted to its backend id.
	Anchors map[AnchorName]BackendID
}

// --- The packed transaction (optimization output) ---------------------------

// NodeStamp is the generation-time write-back provenance a touched node carries,
// before any backend-id resolution: its expected content and property hashes, its
// parent's symbolic id, and its display title. It originates on a node's graph.Op,
// rides every AtomicUnit of the node through packing (so whichever unit — create,
// props, or a content chunk — lands in a bin still exposes it), and is resolved
// into a NodeProvenance (which embeds it and adds the run's minted backend ids) by
// the transport. Threading it as one value keeps the quartet from being re-copied
// field-by-field at each hop from Op to NodeProvenance.
type NodeStamp struct {
	// Hash is the node's expected content hash — the value change detection computed
	// this run, persisted into the `hash` derived column so the next ScanStored
	// hash-skips an unchanged node. Empty for an op that writes no content (a DeleteNode).
	Hash Hash
	// PropHash is the node's expected property hash, gating SetProperties independently
	// of Hash gating SetContent (the two-hash split, #110 phase 2). Empty for a DeleteNode.
	PropHash Hash
	// Parent is the node's parent symbolic id — "" for a top-level area-DB row, else the
	// cluster root a subpage lives under. It routes write-back (a subpage folds into its
	// parent's subtree map) and, on a CreateNode, wires parent-before-child ordering.
	Parent SymbolicID
	// Title is the node's display title, recorded in a subpage's subtree-map entry so a
	// later ScanRecompute can match a live page (which carries only a title, not a repo
	// path) back to its subpath. Empty for a DeleteNode.
	Title string
}

// FillMissing copies each field of o into the receiver only where the receiver's is
// still empty — the first-non-empty fold the optimizer's accumulator applies as it
// gathers a bin's units (every unit of one node carries the same stamp, but a
// content-less node supplies it via its props/create unit rather than a content chunk).
func (s *NodeStamp) FillMissing(o NodeStamp) {
	if s.Hash == "" {
		s.Hash = o.Hash
	}
	if s.PropHash == "" {
		s.PropHash = o.PropHash
	}
	if s.Parent == "" {
		s.Parent = o.Parent
	}
	if s.Title == "" {
		s.Title = o.Title
	}
}

// PackedTxn is the transaction-DAG's node: one sealed backend Transaction plus
// the backend-neutral metadata the transport needs, aggregated by optimization
// (Stage 2) from the AtomicUnits that went into the bin. It is a boundary type —
// transport consumes it — so it lives here in internal/publish rather than inside
// the optimizer, which only holds the opaque Transaction.
//
// The three id fields serve two consumers from one vocabulary: the optimizer
// reads them to wire the transaction-DAG's edges (a T depends on U iff a T.Refs id
// is in U.Produces or U.Anchors), and the transport reads them to gate readiness
// (Refs) and pace (Group) at runtime. Produces here is the SYMBOLIC side, known
// before execution and used only to wire edges; the real symbolic-id → BackendID
// pairs come back after execution in ExecResult (#164 §2).
type PackedTxn struct {
	// Txn is the opaque sealed API call Bin.Build produced; only the backend
	// executes it.
	Txn Transaction
	// Group is the shared affinity key of every unit in the bin — the transport's
	// per-target pacing key.
	Group GroupKey
	// Refs are the exposed unresolved symbolic ids this transaction still needs
	// (the readiness gate), after intra-transaction Ref-suppression: the union of
	// its units' Refs minus anything this same transaction Produces or Anchors.
	// A Ref satisfied inside the same bin creates no edge and is not exposed.
	Refs []SymbolicID
	// Anchors are the named anchors this transaction hosts (union of its units').
	Anchors []AnchorName
	// Produces are the symbolic ids this transaction creates — used for edge
	// derivation and, at runtime, to seed the resolution table from ExecResult.
	Produces []SymbolicID
	// NodeStamp is the write-target node's generation-time write-back provenance
	// (content/property hashes, parent routing, title), threaded through so the
	// transport can assemble Provenance without re-reading source. Every unit of a
	// touched node carries it, so it survives whichever arm packed the bin. Zero for
	// a transaction that writes no node content (e.g. a DeleteNode).
	NodeStamp
}

// --- Publish-time write-back provenance (#167 decision 7) -------------------

// Provenance is what a publish records back into the backend's self-describing
// state so the next ScanStored reads current ids/hashes/anchors. The transport
// assembles it from the run's executed transactions and its resolution table, and
// hands it to a WriteBacker after a successful drain. An unchanged run yields an
// empty Provenance (no nodes touched), so write-back stays a true no-op.
type Provenance struct {
	// Nodes is the per-node provenance for every node this run (re)wrote, keyed by
	// its symbolic id.
	Nodes map[SymbolicID]NodeProvenance
}

// NodeProvenance is one node's write-back record: its real backend id, the
// content hash to store, its parent routing (top-level row vs. subtree-map
// member), and any anchors its content hosts.
type NodeProvenance struct {
	// NodeStamp is the node's unresolved generation-time provenance (content and
	// property hashes, parent symbolic id, title), carried verbatim from the
	// PackedTxn; the fields below add the run's resolved backend ids. Hash, PropHash,
	// Parent and Title read through by field promotion.
	NodeStamp
	// ID is the node's real backend id, resolved from the run (minted for a new
	// node, scan-seeded for a re-asserted existing one).
	ID BackendID
	// ParentID is the parent's resolved backend id (the row whose subtree map a
	// subpage folds into), set only when Parent is non-empty.
	ParentID BackendID
	// Anchors are the anchors this node's content hosts, name → backend block id,
	// written into the node's `anchors` derived column (the glossary role row).
	Anchors map[AnchorName]BackendID
}

// --- The neutral document handoff (tokenizer input) -------------------------

// Document is the backend-neutral, block-granular content a Tokenizer breaks
// into AtomicUnits — the SetContent handoff of #162, projected to exactly what
// the tokenizer boundary needs: an ordered list of block-level nodes plus the
// group they target. It is a minimal placeholder: the rich parsed semantic tree
// (headings, lists, first-class Ref nodes) is #162/graph's to supply, and can
// enrich Block without changing this seam.
type Document struct {
	// Group is the target node this document's content belongs to; every unit
	// tokenized from it inherits this affinity key.
	Group GroupKey
	// Blocks are the ordered block-level nodes of the document.
	Blocks []Block
}

// Block is one block-level node of a neutral Document. Content is opaque neutral
// content the backend reshapes into its own blocks; Refs and Anchors ride along
// so tokenization preserves late-bound references and the anchor map.
type Block struct {
	// Content is opaque neutral block content (#162/graph enriches its vocabulary).
	Content any
	// Refs are the unresolved symbolic ids this block embeds.
	Refs []SymbolicID
	// Anchors are the named anchors this block declares.
	Anchors []AnchorName
}
