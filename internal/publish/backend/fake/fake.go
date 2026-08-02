// Package fake is a fully in-memory backend that implements all four publishing
// role interfaces. It is the primary test harness for the okfpub pipeline:
// because the roles are narrow, an in-memory fake is cheap and composable, and
// makes Generation, Optimization, and Transport testable end-to-end —
// deterministically and offline — as well as each stage testable in isolation
// against one fake role.
//
// A test constructs a *Backend with New (dialing packing pressure via
// WithMaxCount and seeding a canned scan via WithScan) and passes it wherever a
// backend.Backend, or any single role, is required.
//
// See sigma/ideas#172 (ratified #163), decision 7.
package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// Backend is an in-memory implementation of every backend role. Its executor
// state (the mint sequence and the record of executed transactions) is guarded by
// a mutex so it is safe to share across the concurrent workers of a transport
// test. The zero value is not usable; construct with New.
type Backend struct {
	maxCount int                   // per-bin unit ceiling; 0 = unbounded
	scan     *publish.CurrentState // canned Scan result

	mu       sync.Mutex
	seq      int                   // monotonic source of synthetic backend ids
	executed []publish.Transaction // every transaction Execute has sealed, in order
}

// Option configures a Backend built by New.
type Option func(*Backend)

// WithMaxCount bounds every Bin to at most n accepted units, dialing packing
// pressure for a test. n <= 0 leaves bins unbounded (the default).
func WithMaxCount(n int) Option {
	return func(b *Backend) { b.maxCount = n }
}

// WithScan seeds the CurrentState that Scan returns. Without it, Scan returns an
// empty snapshot.
func WithScan(cs *publish.CurrentState) Option {
	return func(b *Backend) { b.scan = cs }
}

// New builds an in-memory fake backend. By default bins are unbounded and Scan
// returns an empty CurrentState.
func New(opts ...Option) *Backend {
	b := &Backend{}
	for _, opt := range opts {
		opt(b)
	}
	if b.scan == nil {
		b.scan = publish.NewCurrentState(nil, nil, nil)
	}
	return b
}

// --- Tokenizer --------------------------------------------------------------

// Tokenize emits one trivial AtomicUnit per block: Cost 1, the document's Group,
// and the block's content, Refs, and Anchors preserved verbatim. It never splits
// or fuses — the simplest possible tokenization, so a Generation/Optimization
// test can reason about exact unit counts.
func (b *Backend) Tokenize(doc publish.Document) []publish.AtomicUnit {
	units := make([]publish.AtomicUnit, 0, len(doc.Blocks))
	for _, blk := range doc.Blocks {
		units = append(units, publish.AtomicUnit{
			Payload: blk.Content,
			Cost:    1,
			Group:   doc.Group,
			Refs:    blk.Refs,
			Anchors: blk.Anchors,
		})
	}
	return units
}

// --- ConstraintModel / Bin --------------------------------------------------

// NewBin returns a fresh count-bounded Bin honoring the backend's MaxCount.
func (b *Backend) NewBin() backend.Bin {
	return &bin{maxCount: b.maxCount}
}

// bin accumulates units of one Group and refuses once its unit count would exceed
// maxCount. It applies no fusion: Build simply seals the accepted units.
type bin struct {
	maxCount int
	units    []publish.AtomicUnit
}

// Add appends a unit unless doing so would exceed maxCount, in which case it
// returns false without mutating the bin. maxCount <= 0 is unbounded.
func (bn *bin) Add(u publish.AtomicUnit) bool {
	if bn.maxCount > 0 && len(bn.units) >= bn.maxCount {
		return false
	}
	bn.units = append(bn.units, u)
	return true
}

// Build seals the accepted units into an opaque Transaction. The bin should not
// be used after Build.
func (bn *bin) Build() publish.Transaction {
	return &transaction{units: bn.units}
}

// transaction is the fake's opaque sealed Transaction: just the units it packed.
type transaction struct {
	units []publish.AtomicUnit
}

// --- Executor ---------------------------------------------------------------

// Execute records the transaction and mints synthetic backend ids for what it
// produced: one BackendID per node the transaction creates (keyed by its units'
// shared Group — the fake's stand-in for the produced node's symbolic id, since
// an AtomicUnit carries no explicit produced-id) and one per hosted anchor. It
// returns those resolution-table updates.
//
// Ref resolution is deliberately not the backend's concern: the transport owns
// the resolution table and gates a transaction's readiness on its Refs before
// ever calling Execute (decision 5); the backend's only Ref duty is the physical
// swap inside its own wire payload, which is a no-op for the fake's opaque
// payloads. So the Resolver goes unused here.
func (b *Backend) Execute(_ context.Context, txn publish.Transaction, _ backend.Resolver) (publish.ExecResult, error) {
	t, ok := txn.(*transaction)
	if !ok {
		return publish.ExecResult{}, fmt.Errorf("fake: Execute got a foreign transaction type %T", txn)
	}

	res := publish.ExecResult{
		Nodes:   map[publish.SymbolicID]publish.BackendID{},
		Anchors: map[publish.AnchorName]publish.BackendID{},
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Mint one node id per distinct Group in the transaction (a fused transaction
	// carries one Group, but this stays correct if a caller mixes them).
	for _, u := range t.units {
		node := publish.SymbolicID(u.Group)
		if _, seen := res.Nodes[node]; !seen {
			res.Nodes[node] = b.mint("node")
		}
		for _, a := range u.Anchors {
			res.Anchors[a] = b.mint("anchor")
		}
	}
	b.executed = append(b.executed, t)
	return res, nil
}

// mint returns a fresh, unique synthetic backend id. Caller must hold b.mu.
func (b *Backend) mint(kind string) publish.BackendID {
	b.seq++
	return publish.BackendID(fmt.Sprintf("fake-%s-%d", kind, b.seq))
}

// Executed returns the transactions Execute has sealed, in order — a test hook for
// asserting what the transport actually wrote. The returned slice is a snapshot.
func (b *Backend) Executed() []publish.Transaction {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]publish.Transaction, len(b.executed))
	copy(out, b.executed)
	return out
}

// --- Scanner ----------------------------------------------------------------

// Scan returns the canned CurrentState the fake was seeded with (empty by
// default).
func (b *Backend) Scan(context.Context) (*publish.CurrentState, error) {
	return b.scan, nil
}
