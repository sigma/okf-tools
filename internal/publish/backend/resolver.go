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

// HostedAnchors collects the set of anchor names a transaction hosts itself, given
// the transaction's ordered write targets and how to read one's declared anchors.
// It returns nil when the transaction hosts none, so the common case is a cheap
// nil check rather than an empty map.
//
// Every backend needs this set, because the optimizer SUPPRESSES a Ref that the
// same transaction satisfies: such a Ref never reaches the transport's readiness
// gate, so the resolution table cannot answer it mid-Execute and the backend is
// silently obliged to answer it locally. That obligation used to be discharged by
// three separate collectors over three payload types, which is how the same defect
// recurred once per medium — sigma/okf-tools#89 and #102 on Notion, #76 on the
// filesystem export, #170 and #171 on Google Docs.
func HostedAnchors[T any](hosts []T, anchorsOf func(T) []publish.AnchorName) map[publish.AnchorName]bool {
	var hosted map[publish.AnchorName]bool
	for _, h := range hosts {
		for _, a := range anchorsOf(h) {
			if hosted == nil {
				hosted = map[publish.AnchorName]bool{}
			}
			hosted[a] = true
		}
	}
	return hosted
}

// MintOverlay layers a transaction's own hosted anchors over a base Resolver,
// minting each one's mid-Execute id through mint. It is the companion to
// HostedAnchors: together they are the whole of "what does a self-hosted anchor
// resolve to while this transaction is executing", leaving each backend only the
// medium-specific answer.
//
// What mint returns differs by how the medium assigns ids, and that difference is
// the entire per-backend part. The filesystem export knows its ids up front and
// mints the real one, so one pass suffices. Notion and Google Docs cannot: a block
// id or a headingId is server-minted and unknowable until after the write, so they
// mint a placeholder here — enough for the citation to render and the transaction
// not to be rejected as unresolvable — and reconcile it to the real id afterwards,
// positionally for Notion, by rendered offset for Docs.
//
// An empty hosted set returns base unwrapped, so a transaction hosting no anchors
// pays nothing.
func MintOverlay(base Resolver, hosted map[publish.AnchorName]bool, mint func(publish.AnchorName) publish.BackendID) Resolver {
	if len(hosted) == 0 {
		return base
	}
	local := make(map[publish.SymbolicID]publish.BackendID, len(hosted))
	for name := range hosted {
		local[publish.AnchorRef(name)] = mint(name)
	}
	return WithOverlay(base, local)
}
