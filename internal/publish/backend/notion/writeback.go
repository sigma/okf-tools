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
//
// It also FORGETS the run's deleted nodes, which is the same obligation read the
// other way: a subpage's description outlives the subpage, so a delete that only
// archived the page left the owning row still naming it (sigma/okf-tools#189).
func (b *Backend) WriteBack(ctx context.Context, prov publish.Provenance) error {
	if err := b.forget(ctx, prov.Deleted); err != nil {
		return err
	}
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

// forget drops the run's deleted nodes from the subtree maps that recorded them,
// so the next scan does not reconstruct a page this run just archived.
//
// Only a cluster subpage needs it. A top-level node IS a row, and archiving a row
// takes it out of the data source's query results, so the record dies with the
// page; a subpage's record lives in someone else's column and does not. A deleted
// node the run's scan never saw in a subtree map — a top-level row, or an
// unclaimed one — therefore has nothing to forget and costs no write.
func (b *Backend) forget(ctx context.Context, deleted []publish.SymbolicID) error {
	records := make([]record, 0, len(deleted))
	archivedRows := map[string]bool{}
	for _, node := range deleted {
		if _, unclaimed := node.Unclaimed(); unclaimed {
			// An unclaimed row is one no record ever named — that is what makes it
			// unclaimed (#135). It has no repo path to look up, and reclaiming it needs
			// no forgetting.
			continue
		}
		r, recorded := b.recordOf(node.Rel())
		if !recorded {
			continue
		}
		if r.own {
			archivedRows[r.row] = true
		}
		records = append(records, r)
	}

	byOwner := map[string][]string{}
	for _, r := range records {
		if r.own || archivedRows[r.row] {
			// Nothing to prune. A node that IS its row has its description archived with
			// it, and so does every entry of a row this same run is archiving — a whole
			// cluster leaving the bundle takes both. Writing to an archived page would be
			// a wasted request at best, and Notion may refuse it outright.
			continue
		}
		byOwner[r.row] = append(byOwner[r.row], r.subpath)
	}
	owners := make([]string, 0, len(byOwner))
	for id := range byOwner {
		owners = append(owners, id)
	}
	sort.Strings(owners)
	for _, ownerID := range owners {
		gone := byOwner[ownerID]
		if err := b.updateSubtree(ctx, ownerID, func(m map[string]subtreeEntry) {
			for _, subpath := range gone {
				delete(m, subpath)
			}
		}); err != nil {
			return fmt.Errorf("notion: write-back: forget %v from %s: %w", gone, ownerID, err)
		}
	}
	return nil
}

// mergeSubtree folds the run's new/updated subpage entries into an owning row's
// `hashes` subtree map — so adding one subpage never clobbers the map's other
// members.
func (b *Backend) mergeSubtree(ctx context.Context, ownerID string, updates map[string]subtreeEntry) error {
	return b.updateSubtree(ctx, ownerID, func(m map[string]subtreeEntry) {
		for subpath, e := range updates {
			m[subpath] = e
		}
	})
}

// updateSubtree read-modify-writes an owning row's `hashes` subtree map: it reads
// the current column, applies edit to it, and PATCHes the result back. Adding an
// entry and removing one are the same operation on the same column and differ only
// in edit, so they share the read, the encode, and the run's memory of what it last
// wrote there — a prune that re-read the column would undo a merge this same run
// had just made, and vice versa.
func (b *Backend) updateSubtree(ctx context.Context, ownerID string, edit func(map[string]subtreeEntry)) error {
	merged, err := b.currentSubtree(ctx, ownerID)
	if err != nil {
		return err
	}
	edit(merged)
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

// rememberRecorders records what the run's scan learned about where each node is
// described — the pairing forget needs and nothing else can supply. A scan replaces
// it wholesale: it is a description of the destination as that scan found it, not an
// accumulation across scans.
func (b *Backend) rememberRecorders(recordedBy map[string]record) {
	b.subtreeMu.Lock()
	b.recordedBy = maps.Clone(recordedBy)
	b.subtreeMu.Unlock()
}

// recordOf reports where the node at path is described, per the run's scan.
func (b *Backend) recordOf(path string) (record, bool) {
	b.subtreeMu.Lock()
	defer b.subtreeMu.Unlock()
	r, ok := b.recordedBy[path]
	return r, ok
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
