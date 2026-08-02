package publish

import "strings"

// This file defines the backend-neutral currency that flows between the three
// okfpub pipeline stages. The pipeline speaks only this vocabulary; every
// backend specific (Notion's block model, its two coupled API limits, its HTTP
// transport) stays behind the role interfaces in the backend subpackage.
//
// See sigma/ideas#172 for the ratified spec (decisions #162 and #163).

// --- Symbolic identity (the generation-time handles of #162) ---------------

// SymbolicID is a generation-time handle keyed by stable identity, never a
// backend id: "node:docs/adr/0002.md", "anchor:glossary/root-kek". Ops that
// produce something mint one; ops that consume embed it as a Ref. The transport
// resolves it against a scan-seeded resolution table.
type SymbolicID string

// BackendID is an opaque real backend identifier (a Notion page/block id, a
// filesystem path, …). It is neutral data to the pipeline: produced by the
// backend, threaded by the transport, never interpreted.
type BackendID string

// AnchorName is a declared named anchor — a bold-lead term hosted inside a node
// (today's anchor-ref target). The backend reports, per AtomicUnit, which
// anchors that unit hosts so the anchor map can be built.
type AnchorName string

// AnchorRefName reports whether a symbolic id is an anchor reference
// ("anchor:<name>") and, if so, the AnchorName it targets. It is the reader
// counterpart to how anchor Refs are minted, kept here in the neutral vocabulary
// so every stage matches an anchor Ref to a producer's Anchors the same way
// (Stage 1 wiring op-DAG edges, Stage 2 wiring transaction-DAG edges).
func AnchorRefName(id SymbolicID) (AnchorName, bool) {
	if rest, ok := strings.CutPrefix(string(id), "anchor:"); ok {
		return AnchorName(rest), true
	}
	return "", false
}

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
