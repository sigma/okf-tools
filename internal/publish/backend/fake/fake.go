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
	written  []publish.Provenance  // every Provenance WriteBack has recorded, in order
	lastMode backend.ScanMode      // the mode of the most recent Scan call
	scanned  bool                  // whether Scan has been called at least once
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

// TokenizeOp mints the single trivial unit for a non-content op: Cost 1 (so a
// count-bounded Bin treats create/properties/delete exactly like a content unit),
// no payload. The fake never fuses, so it carries no create/properties/delete
// payload to distinguish — the optimizer stamps the unit's Group and Refs.
func (b *Backend) TokenizeOp(publish.NonContentOp) publish.AtomicUnit {
	return publish.AtomicUnit{Cost: 1}
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
// default). The fake reconstructs one snapshot, so both scan modes return it
// identically — the mode is a producer concern the in-memory harness has no
// distinct cheap/expensive path for; a Notion-style split is exercised by the
// Notion backend's own tests.
func (b *Backend) Scan(_ context.Context, mode backend.ScanMode) (*publish.CurrentState, error) {
	b.mu.Lock()
	b.lastMode = mode
	b.scanned = true
	b.mu.Unlock()
	return b.scan, nil
}

// LastScanMode reports the mode of the most recent Scan call and whether Scan has
// been called at all — a test hook for asserting that the steady-state default and
// an opt-in recompute reach the scanner as expected.
func (b *Backend) LastScanMode() (backend.ScanMode, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastMode, b.scanned
}

// --- WriteBacker ------------------------------------------------------------

// WriteBack records the run's Provenance so a transport test can assert what the
// publish would persist (ids, hashes, anchors, subpage routing) without a real
// backend. It performs no I/O.
func (b *Backend) WriteBack(_ context.Context, prov publish.Provenance) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.written = append(b.written, prov)
	return nil
}

// WrittenBack returns the Provenance values WriteBack has recorded, in order — a
// test hook for asserting the publish-time write-back. The returned slice is a
// snapshot.
func (b *Backend) WrittenBack() []publish.Provenance {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]publish.Provenance, len(b.written))
	copy(out, b.written)
	return out
}
