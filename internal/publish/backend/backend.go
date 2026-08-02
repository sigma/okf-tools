// Package backend defines the generic, backend-neutral publishing interface.
//
// It is expressed as narrow role interfaces — Tokenizer, ConstraintModel/Bin,
// Executor, and Scanner — each depended on only by the stage that needs it,
// with a Backend umbrella embedding all four for wiring. Concrete backends
// (Notion first, plus an in-memory fake and a filesystem/export target) live in
// subpackages. The interfaces trade in the backend-neutral currency of the
// parent internal/publish package (AtomicUnit, Transaction, ExecResult,
// CurrentState, symbolic ids), which this package imports.
//
// See sigma/ideas#172 (ratified #163).
package backend

import (
	"context"

	"github.com/sigma/okf-tools/internal/publish"
)

// Tokenizer turns a backend-neutral op into the backend's own AtomicUnits — one
// packing loop's worth of currency. Tokenize handles SetContent's block-granular
// Document (applying the backend's constraints: Notion's block model and its
// per-block char caps, by splitting oversized content into several units);
// TokenizeOp handles the three non-content ops (CreateNode / SetProperties /
// DeleteNode), each minting one unit. It preserves every unit's Refs and hosted
// Anchors so late-bound references survive and the anchor map can be built, and it
// never resolves a Ref; resolution is the transport's job.
//
// TokenizeOp is what lets all four op types flow through one AtomicUnit/Bin path
// carrying a real backend Payload and Cost, so backend-specific fusion (Notion's
// POST /pages collapsing create + properties + first content) can live entirely
// inside a Bin. The optimizer stays backend-agnostic: it forwards the neutral
// NonContentOp and then stamps the returned unit's Group, write-target/parent
// Refs, and graph provenance itself — the backend fills only Payload and Cost.
type Tokenizer interface {
	Tokenize(doc publish.Document) []publish.AtomicUnit
	TokenizeOp(op publish.NonContentOp) publish.AtomicUnit
}

// ConstraintModel advertises a backend's capacity behaviorally rather than as
// declarative numbers: it hands out fresh Bins. Behavioral over declarative keeps
// all limit arithmetic behind the seam — a Bin can express Notion's two coupled
// limits, or a stranger backend's ("≤3 of block-type X"), where a scalar ceiling
// could not.
type ConstraintModel interface {
	NewBin() Bin
}

// Bin is a running accumulator for one transaction's worth of AtomicUnits. Add
// reports whether the unit fits (false = adding it would violate a constraint);
// the caller retries it in a fresh Bin. Build seals the accumulated units into
// one opaque, executable Transaction — the point at which backend-specific fusion
// (e.g. Notion collapsing a create + its properties + its first content chunk
// into one POST) happens. A Bin holds units of a single Group; partitioning by
// Group is the optimization strategy's job (#164), not the Bin's.
type Bin interface {
	// Add tries to add a unit, returning false without mutating the Bin if the
	// unit would violate a constraint.
	Add(u publish.AtomicUnit) (ok bool)
	// Build seals the accumulated units into one opaque, executable Transaction.
	Build() publish.Transaction
}

// Executor executes one sealed Transaction, resolving its late-bound Refs as it
// serializes. Ref substitution folds into execution because opaque payloads mean
// only the backend knows where in its wire payload a Ref sits: the transport
// gates on readiness and passes a Resolver in; the backend performs the physical
// swap, then writes — one atomic resolve-then-write. The returned ExecResult
// carries the resolution-table updates the transaction produced.
type Executor interface {
	Execute(ctx context.Context, txn publish.Transaction, r Resolver) (publish.ExecResult, error)
}

// Resolver is the transport's view onto its resolution table: it answers whether
// a symbolic id has a known backend id yet. The transport owns the table and
// decides when a transaction is ready (every Ref present); the backend only reads
// through this interface to perform substitution.
type Resolver interface {
	Resolve(id publish.SymbolicID) (publish.BackendID, bool)
}

// ScanMode selects how a Scanner reconstructs the neutral CurrentState. It is a
// producer-side parameter only (#167 decision 1): both modes return the identical
// CurrentState interface, so Generation, Optimization, and Transport never learn
// which mode ran — the consumer seam fixed in #163 is untouched.
type ScanMode int

const (
	// ScanStored is the cheap steady-state default: one paginated data-source query
	// over the self-describing derived columns, with zero per-page block reads. It
	// catches authored changes but is blind to live-content drift, which is
	// acceptable on the push-triggered path where source is the authority.
	ScanStored ScanMode = iota
	// ScanRecompute is the opt-in full live block walk: it reads every node's live
	// blocks to recompute ContentHash (true drift) and re-derives subpage ids and
	// the anchor map from the live tree, self-healing the staleness the cheap path
	// cannot see. It costs O(nodes) block-list round-trips, so it is never the
	// default.
	ScanRecompute
)

func (m ScanMode) String() string {
	switch m {
	case ScanStored:
		return "stored"
	case ScanRecompute:
		return "recompute"
	default:
		return "ScanMode(?)"
	}
}

// Scanner produces the backend-neutral current-state snapshot that seeds the
// resolution table and drives change detection. #163 fixes only this consumer
// contract; #167 adds the producer-side mode argument (ScanStored default,
// ScanRecompute opt-in) — both modes return the same neutral CurrentState, so the
// mode is invisible past this seam. The backend-specific reconstruction (hashes,
// anchor ids, pagination, the recompute live walk) stays behind it.
type Scanner interface {
	Scan(ctx context.Context, mode ScanMode) (*publish.CurrentState, error)
}

// WriteBacker persists a publish's provenance back into the backend's
// self-describing state so the NEXT ScanStored is accurate (#167 decision 7).
// Self-description only works if the derived columns stay current: after a
// successful drain the transport hands the backend the run's Provenance — the new
// node ids and content hashes, hosted anchor ids, and subpage→parent routing — and
// the backend writes them into its derived columns / subtree map. It is a
// publish-time obligation, not a scan concern; an unchanged run produces empty
// Provenance and writes nothing (the near-noop property holds).
type WriteBacker interface {
	WriteBack(ctx context.Context, prov publish.Provenance) error
}

// Provisioner is an OPTIONAL backend role: a backend whose destination must be
// shaped before writes implements it, and the pipeline calls Provision exactly
// once at the start of a run, before Scan. It is deliberately NOT part of the
// Backend umbrella — backends whose target needs no provisioning (the in-memory
// fake, the filesystem export) simply omit it, and the pipeline skips the step
// via a type assertion. The Notion backend implements it to reconcile the data
// source's columns against schema.json (create missing columns with the Notion
// type mapped from each column's kind) so a fresh data source Just Works instead
// of requiring the columns to be hand-created. Provision must be idempotent: an
// already-provisioned target produces no writes.
type Provisioner interface {
	Provision(ctx context.Context) error
}

// Backend is the umbrella that embeds every role for construction and wiring.
// One concrete backend struct satisfies all of them (sharing, e.g., its HTTP
// client across them); a stage that needs only one role depends on that role, not
// on Backend.
type Backend interface {
	Tokenizer
	ConstraintModel
	Executor
	Scanner
	WriteBacker
}
