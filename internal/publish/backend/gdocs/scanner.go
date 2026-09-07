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
//
// Self-healing is therefore UNCONDITIONAL rather than something --recompute
// turns on: the scan reads every tab's identity marker and adopts the tab
// whatever the sidecar says, which makes the sidecar a cache of hashes over a
// scan that can always be rebuilt rather than the source of truth for identity
// (#180). There is no cheaper mode for a self-heal to be traded against.
func (b *Backend) Scan(ctx context.Context, _ backend.ScanMode) (*publish.CurrentState, error) {
	b.mu.Lock()
	docID, missing := b.docID, b.missing
	b.mu.Unlock()
	if missing {
		// A dry run against a destination that does not exist yet: nothing to read,
		// and the dump should show a first publish.
		return publish.NewCurrentState(nil, nil, nil), nil
	}
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

	// Walk the document once and read everything identity depends on off it: which
	// tabs exist, which node each one claims through its identity marker, and which
	// titles are already taken.
	//
	// The marker is what makes this possible — it is written on every tab precisely
	// so a tab can be recognised without external state (#151), and until #180
	// nothing ever read it back.
	live := map[string]bool{}
	marked := map[string]string{} // rel -> tabId, from the document itself
	owner := map[string]string{}  // tabId -> the rel its marker names
	titles := map[string]string{} // fitted title -> the node holding it
	var walk func([]documentTab)
	walk = func(tabs []documentTab) {
		for _, t := range tabs {
			id := t.TabProperties.TabID
			live[id] = true
			var rel string
			for name := range t.DocumentTab.NamedRanges {
				r, ok := relOfMarker(name)
				// A tab carrying two markers is residue rather than a shape this
				// backend writes (#176), so pick the lowest and pick it the SAME way
				// every scan, rather than at map-iteration random. The node that loses
				// the tie is re-created — with a disambiguated title, since the map
				// below now sees the tab it lost to.
				if ok && (rel == "" || r < rel) {
					rel = r
				}
			}
			if rel != "" {
				marked[rel] = id
				owner[id] = rel
			}
			// Claim the title even for an UNMARKED tab: the API rejects a duplicate
			// title whatever wrote it, so a stray tab a human added still blocks the
			// name. The empty rel never equals a real one, so disambiguate treats it
			// as someone else's (#180).
			if title := t.TabProperties.Title; title != "" {
				titles[title] = rel
			}
			walk(t.ChildTabs)
		}
	}
	walk(doc.Tabs)

	nodeIDs := map[publish.SymbolicID]publish.BackendID{}
	hashes := map[publish.SymbolicID]publish.Hash{}
	propHashes := map[publish.SymbolicID]publish.Hash{}

	b.mu.Lock()
	b.hashes = state.Nodes
	b.titles = titles
	adopt := func(rel, tab string, ns nodeState) {
		b.tabs[rel] = tab
		id := publish.NodeRef(rel)
		nodeIDs[id] = publish.BackendID(tab)
		if ns.Hash != "" {
			hashes[id] = ns.Hash
		}
		if ns.PropHash != "" {
			propHashes[id] = ns.PropHash
		}
	}
	// The sidecar first, for the hashes only it carries: it is what makes an
	// unchanged re-run a noop. A node whose tab a human deleted must be re-created,
	// so its stored state is dropped rather than trusted.
	for rel, ns := range state.Nodes {
		if ns.Tab == "" || !live[ns.Tab] {
			continue
		}
		// Where the document disagrees with the sidecar, the DOCUMENT wins: the
		// marker is on the tab it identifies, while the sidecar is a cache that can
		// be stale or truncated. Both directions of disagreement have to be checked
		// — the node's marker sitting on a DIFFERENT tab, and this tab's marker
		// naming a DIFFERENT node. Checking only the first would adopt the node onto
		// a tab another node also claims, and since every write replaces a tab's
		// whole body, the second to write would silently erase the first.
		if o, ok := owner[ns.Tab]; ok && o != rel {
			continue
		}
		if tab, ok := marked[rel]; ok && tab != ns.Tab {
			continue
		}
		adopt(rel, ns.Tab, ns)
	}
	// Then the tabs the document claims that the loop above did not take — what an
	// interrupted run leaves behind, plus any tab whose sidecar entry disagreed with
	// it. Their content hash is not known to describe what the tab actually holds,
	// so they are adopted WITHOUT one and the run rewrites them: re-writing a tab is
	// cheap and idempotent, whereas re-creating it is the 400 that poisons the
	// destination.
	for rel, tab := range marked {
		if _, done := nodeIDs[publish.NodeRef(rel)]; done {
			continue
		}
		adopt(rel, tab, nodeState{})
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
