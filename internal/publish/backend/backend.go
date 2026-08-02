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

// Tokenizer breaks a backend-neutral Document into the backend's own AtomicUnits
// — one packing loop's worth of currency. It applies the backend's constraints
// (Notion's block model, per-block char caps by splitting oversized content into
// several units) and preserves each block's Refs and hosted Anchors so late-bound
// references survive and the anchor map can be built. It never resolves a Ref;
// resolution is the transport's job.
type Tokenizer interface {
	Tokenize(doc publish.Document) []publish.AtomicUnit
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

// Scanner produces the backend-neutral current-state snapshot that seeds the
// resolution table and drives change detection. #163 fixes only this consumer
// contract; the backend-specific reconstruction (hashes, anchor ids, pagination)
// is #167's meat.
type Scanner interface {
	Scan(ctx context.Context) (*publish.CurrentState, error)
}

// Backend is the umbrella that embeds all four roles for construction and wiring.
// One concrete backend struct satisfies all four (sharing, e.g., its HTTP client
// across them); a stage that needs only one role depends on that role, not on
// Backend.
type Backend interface {
	Tokenizer
	ConstraintModel
	Executor
	Scanner
}
