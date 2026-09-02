package gdocs

import (
	"context"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// Scan reconstructs the neutral CurrentState from one documents.get plus the
// sidecar.
//
// The ScanMode argument is accepted for the interface and deliberately IGNORED.
// On Notion the two modes differ enormously — one paginated query versus O(nodes)
// block-list round-trips — but a Doc is a single file: documents.get with
// includeTabsContent returns every tab's full content in one call, so the "cheap"
// and "live recompute" paths are the same operation (#152). Faking a distinction
// would be a lie with a cost.
func (b *Backend) Scan(ctx context.Context, _ backend.ScanMode) (*publish.CurrentState, error) {
	b.mu.Lock()
	docID := b.docID
	b.mu.Unlock()
	if docID == "" {
		// Not provisioned (the pipeline calls Provision first, so this is a test or a
		// fresh destination): nothing exists yet.
		return publish.NewCurrentState(nil, nil, nil), nil
	}

	state, err := b.readState(ctx)
	if err != nil {
		return nil, err
	}
	doc, err := b.c.getDocument(ctx, docID)
	if err != nil {
		return nil, err
	}

	// Which tabs actually exist right now. A node whose tab a human deleted must be
	// re-created, so its stored state is dropped rather than trusted.
	live := map[string]bool{}
	var walk func([]documentTab)
	walk = func(tabs []documentTab) {
		for _, t := range tabs {
			live[t.TabProperties.TabID] = true
			walk(t.ChildTabs)
		}
	}
	walk(doc.Tabs)

	nodeIDs := map[publish.SymbolicID]publish.BackendID{}
	hashes := map[publish.SymbolicID]publish.Hash{}
	propHashes := map[publish.SymbolicID]publish.Hash{}

	b.mu.Lock()
	b.hashes = state.Nodes
	for rel, ns := range state.Nodes {
		if ns.Tab == "" || !live[ns.Tab] {
			continue
		}
		b.tabs[rel] = ns.Tab
		id := publish.NodeRef(rel)
		nodeIDs[id] = publish.BackendID(ns.Tab)
		if ns.Hash != "" {
			hashes[id] = ns.Hash
		}
		if ns.PropHash != "" {
			propHashes[id] = ns.PropHash
		}
	}
	// A document Docs just created already holds one empty default tab. If no node
	// claims a tab yet, the first page adopts that one rather than leaving a stray
	// "Tab 1" beside the real content.
	if len(nodeIDs) == 0 && len(doc.Tabs) > 0 {
		b.adoptable = doc.Tabs[0].TabProperties.TabID
	}
	b.mu.Unlock()

	// Anchors are not reconstructed yet: the placeholder tokenizer hosts none, and
	// the real anchor map arrives with the two-pass heading write (#160).
	return publish.NewCurrentStateWithProps(nodeIDs, hashes, propHashes, nil), nil
}
