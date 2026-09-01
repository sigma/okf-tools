package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"sort"

	"github.com/sigma/okf-tools/internal/publish"
)

// WriteBack persists a publish's provenance into Notion's self-describing derived
// columns so the next ScanStored reads current ids/hashes/anchors (#167 decision
// 7). It is a publish-time obligation the transport triggers after a successful
// drain; an unchanged run hands it empty Provenance and it writes nothing.
//
// It routes each node by its parent:
//
//   - a top-level node (no parent) has its own data-source row: WriteBack PATCHes
//     the row's `path` and `hash` columns (and, for the glossary-role row, its
//     `anchors` map). This is a cheap property write, never a body rewrite.
//   - a cluster subpage (a parent) has no row of its own: its {id, hash} folds into
//     its OWNING ROW's `hashes` subtree map (#146), merged with whatever else that map
//     already held via a read-modify-write. The owning row is the nearest ancestor
//     that is a row, which for a one-level cluster is the subpage's parent and for a
//     nested one is further up (#141) — the distinction the parent-kind rule forces,
//     since only a row carries the derived columns.
func (b *Backend) WriteBack(ctx context.Context, prov publish.Provenance) error {
	// Group subpages by their OWNING ROW so each row's subtree map is updated in one
	// merged write, and collect the top-level nodes for their own-row writes.
	subByOwner := map[publish.BackendID]map[string]subtreeEntry{}
	var topLevel []publish.SymbolicID

	for node, np := range prov.Nodes {
		if np.Parent == "" {
			topLevel = append(topLevel, node)
			continue
		}
		// The record climbs to the nearest ancestor ROW, which is the only page that
		// has the derived columns to hold it. Its immediate parent may be a child_page
		// — a nested cluster index — and writing a column there is the #104/#128 400
		// (sigma/okf-tools#141).
		owner := np.OwnerID
		if owner == "" {
			return fmt.Errorf("notion: write-back: subpage %s is recorded by %s, which did not resolve to a row", node, np.Owner)
		}
		m := subByOwner[owner]
		if m == nil {
			m = map[string]subtreeEntry{}
			subByOwner[owner] = m
		}
		// A subpage's anchors ride its entry, since it has no row to carry an anchors
		// column (#142).
		m[node.Rel()] = subtreeEntry{
			ID: string(np.ID), Hash: string(np.Hash), PropHash: string(np.PropHash),
			Title: np.Title, Anchors: anchorMap(np.Anchors),
		}
	}

	// Own-row writes for top-level nodes, in sorted order for deterministic request
	// streams (offline tests assert on the recorded calls).
	sort.Slice(topLevel, func(i, j int) bool { return topLevel[i] < topLevel[j] })
	for _, node := range topLevel {
		np := prov.Nodes[node]
		props := map[string]any{
			"path": b.richTextProp(node.Rel()),
			// The `hash` column stores content and property hashes as one compound
			// value, so the two-hash split needs no new Notion column (#110 phase 2).
			"hash": b.richTextProp(encodeHashPair(np.Hash, np.PropHash)),
		}
		if len(np.Anchors) > 0 {
			enc, err := json.Marshal(anchorMap(np.Anchors))
			if err != nil {
				return fmt.Errorf("notion: write-back: encode anchors for %s: %w", node, err)
			}
			props["anchors"] = b.richTextProp(string(enc))
		}
		if err := b.patchProps(ctx, string(np.ID), props); err != nil {
			return fmt.Errorf("notion: write-back %s: %w", node, err)
		}
	}

	// Subtree-map writes: read-modify-write each owning row's `hashes` column so a
	// new subpage's {id, hash} is added without dropping the map's other entries.
	owners := make([]publish.BackendID, 0, len(subByOwner))
	for id := range subByOwner {
		owners = append(owners, id)
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i] < owners[j] })
	for _, ownerID := range owners {
		if err := b.mergeSubtree(ctx, string(ownerID), subByOwner[ownerID]); err != nil {
			return fmt.Errorf("notion: write-back subtree of %s: %w", ownerID, err)
		}
	}
	return nil
}

// mergeSubtree read-modify-writes an owning row's `hashes` subtree map: it reads the
// current column, folds in the run's new/updated subpage entries, and PATCHes the
// merged map back — so adding one subpage never clobbers the map's other members.
func (b *Backend) mergeSubtree(ctx context.Context, ownerID string, updates map[string]subtreeEntry) error {
	merged, err := b.currentSubtree(ctx, ownerID)
	if err != nil {
		return err
	}
	for subpath, e := range updates {
		merged[subpath] = e
	}
	enc, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("encode subtree map: %w", err)
	}
	if err := b.patchProps(ctx, ownerID, map[string]any{"hashes": b.richTextProp(string(enc))}); err != nil {
		return err
	}
	b.rememberSubtree(ownerID, merged)
	return nil
}

// currentSubtree reports an owning row's subtree map as this run last left it: from
// the run's own memory when it has already merged into that row, otherwise read
// from Notion.
//
// The memory is what makes per-group write-back (#135) safe. Write-back used to run
// once, merging every subpage of a parent in a single read-modify-write; it now runs
// as each subpage's group completes, so a parent with N subpages merges N times. Re-
// reading the column each time would make correctness depend on Notion serving a
// write this same run issued moments earlier — a read-after-write assumption the API
// does not promise — and would spend N extra requests to learn something the run
// already knows. The map is per-Backend and so per-run: it never outlives the writes
// it describes.
func (b *Backend) currentSubtree(ctx context.Context, ownerID string) (map[string]subtreeEntry, error) {
	b.subtreeMu.Lock()
	cached, ok := b.subtrees[ownerID]
	b.subtreeMu.Unlock()
	if ok {
		return maps.Clone(cached), nil
	}

	current, err := b.getPageProps(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	return storedSubtree(plainText(current["hashes"]), ownerID)
}

// rememberSubtree records the map this run just wrote to an owning row, so the next
// merge into that row starts from it rather than from a re-read.
func (b *Backend) rememberSubtree(ownerID string, merged map[string]subtreeEntry) {
	b.subtreeMu.Lock()
	if b.subtrees == nil {
		b.subtrees = map[string]map[string]subtreeEntry{}
	}
	b.subtrees[ownerID] = maps.Clone(merged)
	b.subtreeMu.Unlock()
}

// getPageProps reads a page's properties via GET /pages/{id} — the read half of
// the subtree-map read-modify-write.
func (b *Backend) getPageProps(ctx context.Context, pageID string) (map[string]property, error) {
	var page queryRow
	if err := b.do(ctx, http.MethodGet, "/pages/"+url.PathEscape(pageID), nil, &page); err != nil {
		return nil, err
	}
	return page.Properties, nil
}

// patchProps PATCHes a page's properties (the derived-column write). Notion merges
// the given properties into the page, leaving untouched columns (title, type, …) as
// they were.
//
// This is the third page-property write path, and the parent-kind rule binds it too:
// a page-parented node is a child_page with none of the data source's columns, so
// writing one to it 400s (#104, #128). It does not route through pageProps because
// WriteBack never hands it a subpage — the routing above sends a subpage's record
// into its PARENT row's `hashes` map, so every id reaching here is a top-level row.
// Any future caller must preserve that, or send its properties through pageProps.
func (b *Backend) patchProps(ctx context.Context, pageID string, props map[string]any) error {
	return b.do(ctx, http.MethodPatch, "/pages/"+url.PathEscape(pageID), updatePageReq{Properties: props}, nil)
}

// richTextProp builds a Notion rich_text property value from s — the shape the
// self-describing derived columns use. It splits s into as many spans as the
// per-span char cap requires, so a value over Notion's 2000-char limit (the
// glossary host's anchors map is the first derived column to hit it, #94) is
// chunked rather than 400ing. The spans concatenate on read (plainText), so
// round-trip reads are unaffected.
func (b *Backend) richTextProp(s string) map[string]any {
	return map[string]any{"rich_text": b.richTextSpans(s)}
}

// anchorMap projects a resolved anchor table to the plain name → id map the
// `anchors` column stores.
func anchorMap(anchors map[publish.AnchorName]publish.BackendID) map[string]string {
	out := make(map[string]string, len(anchors))
	for name, id := range anchors {
		out[string(name)] = string(id)
	}
	return out
}
