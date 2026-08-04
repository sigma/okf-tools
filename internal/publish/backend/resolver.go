package backend

import "github.com/sigma/okf-tools/internal/publish"

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
