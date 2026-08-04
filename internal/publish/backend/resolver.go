package backend

import (
	"fmt"

	"github.com/sigma/okf-tools/internal/publish"
)

// ResolvedRun is a Run with its late-bound Ref already resolved to a real backend
// id. RefID is meaningful only when Run.Ref is non-empty (a reference run); for a
// literal or link run it is the zero BackendID.
type ResolvedRun struct {
	Run   publish.Run
	RefID publish.BackendID
}

// ResolveRuns performs the physical Ref→BackendID substitution for a run slice,
// resolving every reference run through the Resolver and returning the runs paired
// with their resolved ids. An unresolved Ref is an error — the transport gates a
// transaction on its Refs being resolvable before Execute, so a miss here is a
// contract violation, not a data condition. It concentrates the resolve-and-error
// half both backends share; each then emits from the ResolvedRuns in its own medium
// (fs Markdown links, notion page mentions) without re-resolving.
func ResolveRuns(runs []publish.Run, r Resolver) ([]ResolvedRun, error) {
	out := make([]ResolvedRun, 0, len(runs))
	for _, run := range runs {
		rr := ResolvedRun{Run: run}
		if run.Ref != "" {
			id, ok := r.Resolve(run.Ref)
			if !ok {
				return nil, fmt.Errorf("content ref %s did not resolve", run.Ref)
			}
			rr.RefID = id
		}
		out = append(out, rr)
	}
	return out, nil
}

// overlayResolver resolves an id against a transaction-local overlay first, falling
// back to the base Resolver. The overlay only ever holds a transaction's own
// just-hosted anchors — ids the transport table cannot yet resolve mid-Execute;
// every other id (parents, cross-document links) falls straight through to base.
type overlayResolver struct {
	local map[publish.SymbolicID]publish.BackendID
	base  Resolver
}

func (o overlayResolver) Resolve(id publish.SymbolicID) (publish.BackendID, bool) {
	if b, ok := o.local[id]; ok {
		return b, true
	}
	return o.base.Resolve(id)
}

// WithOverlay layers a transaction-local resolution map over a base Resolver: an id
// present in local resolves to its overlay BackendID, every other id falls through
// to base. Both backends use it to resolve their own just-hosted anchors mid-Execute
// — the local resolution the optimizer's self-anchor suppression assumes — the fs
// backend with deterministic on-disk ids computed up front, the notion backend with
// server-minted block ids learned after the write. The overlay is keyed by
// SymbolicID (the resolver's own key), so a caller holding anchor names converts
// them with publish.AnchorRef when building it. An empty local returns base
// unwrapped, so the common no-anchor case pays nothing.
func WithOverlay(base Resolver, local map[publish.SymbolicID]publish.BackendID) Resolver {
	if len(local) == 0 {
		return base
	}
	return overlayResolver{local: local, base: base}
}
