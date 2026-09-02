package gdocs

import (
	"context"
	"strings"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// Scan reconstructs the neutral CurrentState from ONE documents.get plus the
// Drive file's appProperties.
//
// FIGHT (the ScanMode distinction collapses). The seam offers a cheap stored scan
// and an expensive live recompute, because on Notion the difference is one
// paginated query versus O(nodes) block-list round-trips. A Doc is a single file:
// documents.get with includeTabsContent=true returns EVERY tab's full content in
// one call, so the live walk costs the same as the cheap path. ScanStored and
// ScanRecompute are the same operation here, and the mode argument is inert.
func (b *Backend) Scan(_ context.Context, _ backend.ScanMode) (*publish.CurrentState, error) {
	b.svc.gets++

	nodeIDs := map[publish.SymbolicID]publish.BackendID{}
	hashes := map[publish.SymbolicID]publish.Hash{}
	propHashes := map[publish.SymbolicID]publish.Hash{}
	anchorIDs := map[publish.AnchorName]publish.BackendID{}

	for key, val := range b.svc.appProps {
		switch {
		case strings.HasPrefix(key, "okf:"):
			rel := strings.TrimPrefix(key, "okf:")
			nodeIDs[publish.NodeRef(rel)] = publish.BackendID(val)
			b.pathToTab[rel] = val
		case strings.HasPrefix(key, "hash:"):
			rel := strings.TrimPrefix(key, "hash:")
			hashes[publish.NodeRef(rel)] = publish.Hash(val)
		case strings.HasPrefix(key, "phash:"):
			rel := strings.TrimPrefix(key, "phash:")
			propHashes[publish.NodeRef(rel)] = publish.Hash(val)
		}
	}
	for _, t := range b.svc.tabs {
		for name, id := range t.headingIDs {
			anchorIDs[name] = publish.BackendID(id)
		}
	}
	return publish.NewCurrentStateWithProps(nodeIDs, hashes, propHashes, anchorIDs), nil
}

// appPropertyLimit is Drive's documented ceiling on ONE appProperty: 124 bytes for
// key and value combined.
const appPropertyLimit = 124

// maxAppProperties is Drive's documented ceiling on private properties per app.
const maxAppProperties = 30

// WriteBack persists per-node content hashes so the next Scan can hash-skip.
//
// FIGHT (there is nowhere to put per-node state). Notion writes provenance into
// derived columns on each row — unbounded, one per node. Drive gives the file 30
// private properties of 124 bytes each, total. One property per node overflows at
// 30 pages; packing every hash into one property overflows at roughly three. The
// prototype writes them anyway and reports the overflow, which is the evidence
// that per-tab state needs a different home entirely.
func (b *Backend) WriteBack(_ context.Context, prov publish.Provenance) error {
	for id, np := range prov.Nodes {
		if np.Hash == "" {
			continue
		}
		key := "hash:" + id.Rel()
		if len(key)+len(np.Hash) > appPropertyLimit {
			b.overflow = append(b.overflow, key+" (too long)")
			continue
		}
		b.svc.appProps[key] = string(np.Hash)
		// The property hash needs its OWN property, or SetProperties can never
		// hash-skip and every run rewrites every tab. Two properties per node against
		// Drive's 30 caps the whole bundle at fifteen pages.
		if np.PropHash != "" {
			b.svc.appProps["phash:"+id.Rel()] = string(np.PropHash)
		}
	}
	if n := len(b.svc.appProps); n > maxAppProperties {
		b.overflow = append(b.overflow, "appProperties count exceeded")
	}
	return nil
}
