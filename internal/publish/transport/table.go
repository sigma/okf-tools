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
// per-merge safety. That is what the concurrent drain needs (#212): a worker's
// lookups — inside Execute, and in the write-back that follows — run while other
// workers merge, and a lookup can only ever see a merge as absent or complete,
// never half-written. Readiness itself is decided on the dispatcher's goroutine
// between wavefronts, when no merge is in flight, so it needs no coarser gate.
type table struct {
	seed *publish.CurrentState
	// produced is every symbolic id some transaction of this run PRODUCES. Such an
	// id is resolved by its producer and never by the seed: a node re-created under a
	// new parent (sigma/okf-tools#209) is still in the seed under its path, mapped to
	// the page this run is archiving, and a consumer that read the seed would link to
	// it. Masking the seed makes the consumer wait for the create — the ordering the
	// op-DAG's content-refs-node edge already expresses, now honoured by the gate.
	produced map[publish.SymbolicID]bool

	mu      sync.Mutex
	nodes   map[publish.SymbolicID]publish.BackendID
	anchors map[publish.AnchorName]publish.BackendID
}

func newTable(seed *publish.CurrentState, produced []publish.SymbolicID) *table {
	t := &table{
		seed:     seed,
		produced: map[publish.SymbolicID]bool{},
		nodes:    map[publish.SymbolicID]publish.BackendID{},
		anchors:  map[publish.AnchorName]publish.BackendID{},
	}
	for _, id := range produced {
		t.produced[id] = true
	}
	return t
}

// Resolve reports the backend id a symbolic id resolves to, if any. An
// "anchor:<name>" id resolves against the anchor table (then the seed's
// AnchorID); an "unclaimed:<id>" resolves to the id it carries; any other id
// resolves against the node table (then the seed's NodeID, unless something this
// run produces it). This is the backend.Resolver contract.
func (r *table) Resolve(id publish.SymbolicID) (publish.BackendID, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// An unclaimed object is keyed by its backend id because that is the only
	// identity it has (#135), so the ref IS its resolution. A scan that minted the
	// ref seeds the same pair; a re-parent's archive (#209) mints one for a page the
	// scan knew under a path that now names the page's replacement, and no seed
	// carries that.
	if b, ok := id.Unclaimed(); ok {
		return b, true
	}

	if name, ok := id.AnchorName(); ok {
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
	if r.seed != nil && !r.produced[id] {
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
