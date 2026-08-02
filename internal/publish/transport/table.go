package transport

import (
	"maps"
	"sync"

	"github.com/sigma/okf-tools/internal/publish"
)

// table is the transport's resolution table: the scan seed (a CurrentState)
// layered under the symbolic-id → BackendID pairs merged from each ExecResult.
// A lookup consults the merged updates first and falls back to the seed, so a Ref
// to an already-existing target resolves straight from the scan with no edge,
// while a Ref to something created this run resolves once its producer executes.
//
// table implements backend.Resolver, so the transport hands it directly to
// Execute; the backend reads through Resolve to perform the physical ref swap.
// Each method takes the mutex for its own duration, giving per-lookup and
// per-merge safety — enough for today's sequential drain (readiness is gated
// before any Execute, so no lookup races a merge) and a foundation a future
// concurrent drain can build a coarser gate-level lock on.
type table struct {
	seed *publish.CurrentState

	mu      sync.Mutex
	nodes   map[publish.SymbolicID]publish.BackendID
	anchors map[publish.AnchorName]publish.BackendID
}

func newTable(seed *publish.CurrentState) *table {
	return &table{
		seed:    seed,
		nodes:   map[publish.SymbolicID]publish.BackendID{},
		anchors: map[publish.AnchorName]publish.BackendID{},
	}
}

// Resolve reports the backend id a symbolic id resolves to, if any. An
// "anchor:<name>" id resolves against the anchor table (then the seed's
// AnchorID); any other id resolves against the node table (then the seed's
// NodeID). This is the backend.Resolver contract.
func (r *table) Resolve(id publish.SymbolicID) (publish.BackendID, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if name, ok := publish.AnchorRefName(id); ok {
		if b, ok := r.anchors[name]; ok {
			return b, true
		}
		if r.seed != nil {
			return r.seed.AnchorID(name)
		}
		return "", false
	}
	if b, ok := r.nodes[id]; ok {
		return b, true
	}
	if r.seed != nil {
		return r.seed.NodeID(id)
	}
	return "", false
}

// merge folds an ExecResult's new pairs into the table.
func (r *table) merge(res publish.ExecResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, b := range res.Nodes {
		r.nodes[id] = b
	}
	for name, b := range res.Anchors {
		r.anchors[name] = b
	}
}

// resolves reports whether every ref currently resolves — the readiness gate.
func (r *table) resolves(refs []publish.SymbolicID) bool {
	for _, ref := range refs {
		if _, ok := r.Resolve(ref); !ok {
			return false
		}
	}
	return true
}

// unresolved returns the subset of refs that do not yet resolve, for diagnostics.
func (r *table) unresolved(refs []publish.SymbolicID) []publish.SymbolicID {
	var out []publish.SymbolicID
	for _, ref := range refs {
		if _, ok := r.Resolve(ref); !ok {
			out = append(out, ref)
		}
	}
	return out
}

// nodesCopy and anchorsCopy return defensive clones of the merged tables so the
// transport can hand them out in a Result without exposing its live maps.
func (r *table) nodesCopy() map[publish.SymbolicID]publish.BackendID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.nodes)
}

func (r *table) anchorsCopy() map[publish.AnchorName]publish.BackendID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.anchors)
}
