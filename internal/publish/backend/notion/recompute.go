package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"

	"github.com/sigma/okf-tools/internal/publish"
)

// scanRecompute is the opt-in ScanRecompute producer: the full live block walk
// (#167 decision 5). It still enumerates owned rows with the one paginated
// data-source query, but then, for every node, reads its live block tree to
//
//   - recompute ContentHash from live content (the true drift signal), and
//   - re-derive subpage ids (from the live child_page walk) and the anchor map
//     (from the live glossary blocks), self-healing the staleness the cheap
//     ScanStored path cannot see (a subpage re-parented in Notion, an anchor whose
//     host block moved).
//
// It costs O(nodes) block-list round-trips plus their pagination — the full price
// the drift sweep pays and an ordinary push never does. It emits the same neutral
// CurrentState as ScanStored, so the consumer seam is untouched.
//
// Hash provenance note: the live ContentHash is a fingerprint of the reconstructed
// live content, computed by liveContentHash. Making it byte-identical to the
// source-side hash (so an unchanged node hash-skips under recompute too) requires a
// shared normalization across the Notion round-trip — the seam is graph.WithHasher
// on the source side paired with this serializer on the scan side; wiring that
// exact alignment is deferred (see the ticket report). Independent of alignment,
// recompute's id re-derivation is the concrete self-heal this mode delivers.
func (b *Backend) scanRecompute(ctx context.Context) (*publish.CurrentState, error) {
	rows, err := b.queryAllRows(ctx)
	if err != nil {
		return nil, err
	}

	nodeIDs := map[publish.SymbolicID]publish.BackendID{}
	hashes := map[publish.SymbolicID]publish.Hash{}
	propHashes := map[publish.SymbolicID]publish.Hash{}
	anchorIDs := map[publish.AnchorName]publish.BackendID{}
	owner := map[string]string{}
	claim := func(path, by string) error {
		if prev, dup := owner[path]; dup {
			return fmt.Errorf("notion: scan: path %q claimed by two pages (%s and %s)", path, prev, by)
		}
		owner[path] = by
		return nil
	}

	for _, row := range rows {
		path := plainText(row.Properties["path"])
		if path == "" {
			// Unclaimed: no path, so no node — reclaimed rather than walked (#135).
			markUnclaimed(nodeIDs, row.ID)
			continue
		}
		if err := claim(path, row.ID); err != nil {
			return nil, err
		}
		node := publish.NodeRef(path)
		nodeIDs[node] = publish.BackendID(row.ID)

		blocks, err := b.listLiveBlocks(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		// True drift: hash the live content of this row's own blocks. The property
		// hash is not reconstructed from live properties — recompute reads it back
		// from the stored compound `hash` column (grilling decision 5), so live
		// reconstruction stays scoped to body content only.
		hashes[node] = liveContentHash(blocks)
		if _, prop := decodeHashPair(plainText(row.Properties["hash"])); prop != "" {
			propHashes[node] = prop
		}

		// Subpages: re-derive each cluster subpage's id from the live child_page
		// walk (self-heal), falling back to the stored subtree map for any subpath
		// the live walk did not surface. A live child_page carries only a page id and
		// a title (never the repo path), so it is matched back to a stored subpath by
		// id (unchanged) or, when the id has gone stale, by the stored title. A
		// child_page matching neither is not one we own by record and is skipped —
		// never given a fabricated subpath id.
		stored, err := storedSubtree(plainText(row.Properties["hashes"]), row.ID)
		if err != nil {
			return nil, err
		}
		byID, byTitle := subtreeIndexes(stored)
		walk := &subtreeWalk{
			be: b, rowID: row.ID, stored: stored, byID: byID, byTitle: byTitle,
			claim: claim, nodeIDs: nodeIDs, hashes: hashes, propHashes: propHashes,
		}
		if err := walk.descend(ctx, row.ID, blocks); err != nil {
			return nil, err
		}
		for subpath, e := range stored {
			if err := claim(subpath, "subtree of "+row.ID); err != nil {
				return nil, err
			}
			sub := publish.NodeRef(subpath)
			if e.ID != "" {
				nodeIDs[sub] = publish.BackendID(e.ID)
			}
			if e.Hash != "" {
				hashes[sub] = publish.Hash(e.Hash)
			}
			if e.PropHash != "" {
				propHashes[sub] = publish.Hash(e.PropHash)
			}
		}

		// Anchors: a row carrying an anchors column is the glossary-role row. Its
		// recorded map is the seed — the same one ScanStored reads — but here it is
		// VALIDATED against the page's live children before being trusted, which is the
		// drift detection this mode exists for (#138).
		//
		// It is not re-derived from live content. A live block carries no marker naming
		// the anchor it hosts: the mirror stores no emphasis (the executor writes plain
		// text spans), so there is nothing on the page to key a name to. An earlier
		// attempt keyed on a leading bold run, which no published block has ever
		// carried — so the map came back empty on every mirror and every anchor
		// citation in the corpus went unresolvable.
		dangling, err := seedRecordedAnchors(row, blocks, anchorIDs)
		if err != nil {
			return nil, err
		}
		if dangling {
			// Withhold the live hash computed above. Change detection cannot confirm
			// "unchanged" without one, so it re-asserts this host — and re-asserting is
			// the one path that re-mints and re-records the anchor map.
			delete(hashes, node)
		}
	}

	return publish.NewCurrentStateWithProps(nodeIDs, hashes, propHashes, anchorIDs), nil
}

// seedRecordedAnchors seeds a glossary row's recorded anchor map into anchorIDs and
// reports whether that map is DANGLING — the caller's cue to withhold the host's
// content hash so the host re-asserts.
//
// An anchor is dangling when the block id recorded for it is no longer a child of
// the host page: the state an interrupted run leaves, since a content assertion
// replaces a page's children and so mints new ids (#130) while the record of the old
// ones survives a write-back that never ran (#135).
//
// A dangling map suppresses the WHOLE seed for that host, not just the dead entries.
// Re-asserting replaces every block on the page, so seeding the survivors would let
// a citing page resolve to a block that is about to be deleted — precisely the
// corruption being repaired. Suppressed, the citing page has no seeded id, so it
// waits on the host's re-assertion and takes the fresh ones.
//
// The trade this makes: if the host is NOT re-asserted in the same run — it fell out
// of publish scope, say — a citing page's anchor ref resolves to nothing and the run
// stops with "transactions cannot proceed" instead of proceeding. That is the better
// failure. The alternative is a run that succeeds while minting mentions of blocks it
// then deletes, which is how the mirror got here. The ordinary path does not hit it:
// withholding the hash guarantees the host re-asserts, which is what
// TestRecomputeReassertsAHostWithDanglingAnchors drives end to end.
//
// Validation is against the host's DIRECT children, which is where every anchor it
// hosts lives: the executor writes a node's body as a flat list of child blocks and
// reports hosted anchors per child, so an anchor id is always one of them. A nested
// block could never be matched here and would read as permanently dangling.
func seedRecordedAnchors(row queryRow, blocks []liveBlock, anchorIDs map[publish.AnchorName]publish.BackendID) (dangling bool, err error) {
	raw := plainText(row.Properties["anchors"])
	if raw == "" {
		return false, nil
	}
	recorded := map[publish.AnchorName]publish.BackendID{}
	if err := readAnchors(raw, recorded); err != nil {
		return false, err
	}

	live := make(map[string]bool, len(blocks))
	for _, blk := range blocks {
		live[blk.id] = true
	}
	for _, id := range recorded {
		if !live[string(id)] {
			return true, nil
		}
	}
	maps.Copy(anchorIDs, recorded)
	return false, nil
}

// subtreeWalk carries the state one row's live subtree walk threads through its
// recursion: what the row RECORDS (stored, and the two reverse lookups into it) and
// where the walk writes what it finds. It exists so the descent can recurse without
// passing eight arguments down each level.
type subtreeWalk struct {
	be *Backend
	// What the row records: the subtree map, and the two reverse lookups a live
	// child_page is matched through (by id, or by title once the id has gone stale).
	rowID   string
	stored  map[string]subtreeEntry
	byID    map[string]string
	byTitle map[string]string
	// claim guards the mirror's 1:1 path invariant across the whole scan, and doubles
	// as this walk's cycle guard — see descend.
	claim func(path, by string) error

	// Where the walk writes what it finds.
	nodeIDs    map[publish.SymbolicID]publish.BackendID
	hashes     map[publish.SymbolicID]publish.Hash
	propHashes map[publish.SymbolicID]publish.Hash
}

// descend walks the child_page blocks of one page, recording each subpage it
// recognises and then recursing into it.
//
// It recurses because a mirror nests deeper than one level: a cluster index is a
// page like any other and may hold subpages of its own, and every one of them is
// recorded in the same row's map, which is what makes them reachable from here
// (sigma/okf-tools#141). Walking only the row's direct children would leave a
// grandchild's live content unread while the scan still claimed to cover it, so the
// mode would report a stored hash as though it had been verified live.
//
// A live child_page carries only a page id and a title, never the repo path, so it is
// matched back to a recorded subpath by id (unchanged) or, when the id has gone
// stale, by the recorded title. A child_page matching neither is not one we own by
// record and is skipped — never given a fabricated subpath id.
//
// TERMINATION rests on claim: a subpath can be claimed once, so a live tree that
// somehow cycles (a page reachable from itself) fails loudly on the second claim
// instead of recursing forever. The walk itself imposes no depth limit, because the
// legitimate depth is whatever the source hierarchy has. Each descent names its
// containing page as the claimant, so the error identifies the two places a path was
// reached from rather than naming the row twice.
func (w *subtreeWalk) descend(ctx context.Context, pageID string, blocks []liveBlock) error {
	for _, blk := range blocks {
		if blk.typ != nTypeChildPage {
			continue
		}
		subpath, ok := w.byID[blk.id]
		if !ok {
			subpath, ok = w.byTitle[blk.childPageTitle]
		}
		if !ok {
			continue
		}
		if err := w.claim(subpath, "child of "+pageID+" (subtree of "+w.rowID+")"); err != nil {
			return err
		}
		sub := publish.NodeRef(subpath)
		w.nodeIDs[sub] = publish.BackendID(blk.id) // live id, healing a stale stored one
		subBlocks, err := w.be.listLiveBlocks(ctx, blk.id)
		if err != nil {
			return err
		}
		w.hashes[sub] = liveContentHash(subBlocks)
		// The subpage's property hash rides its recorded subtree entry (stored-
		// readback), even as its content hash is reconstructed live.
		if e, ok := w.stored[subpath]; ok && e.PropHash != "" {
			w.propHashes[sub] = publish.Hash(e.PropHash)
		}
		delete(w.stored, subpath)

		if err := w.descend(ctx, blk.id, subBlocks); err != nil {
			return err
		}
	}
	return nil
}

// subtreeIndexes builds the reverse lookups the live child_page walk matches
// against: stored page id → subpath (the id-unchanged case) and stored title →
// subpath (the id-went-stale case, healed by title). Entries without a stored
// title contribute only to the id index.
func subtreeIndexes(stored map[string]subtreeEntry) (byID, byTitle map[string]string) {
	byID = map[string]string{}
	byTitle = map[string]string{}
	for subpath, e := range stored {
		if e.ID != "" {
			byID[e.ID] = subpath
		}
		if e.Title != "" {
			byTitle[e.Title] = subpath
		}
	}
	return byID, byTitle
}

// storedSubtree parses a row's `hashes` subtree-map column into subpath → entry,
// used as the fallback for subpaths the live child_page walk did not surface.
func storedSubtree(raw, rowID string) (map[string]subtreeEntry, error) {
	if raw == "" {
		return map[string]subtreeEntry{}, nil
	}
	var sub map[string]subtreeEntry
	if err := json.Unmarshal([]byte(raw), &sub); err != nil {
		return nil, fmt.Errorf("notion: scan: row %s hashes column: %w", rowID, err)
	}
	return sub, nil
}

// liveBlock is the sliver of a live Notion block ScanRecompute needs: its id (which
// a recorded anchor is validated against), its type, whether it is a child_page (and
// that page's title, the subpath key), and the concatenated plain text of its
// rich_text (the content fingerprint).
//
// It deliberately carries no emphasis. The mirror stores none — the executor writes
// plain text spans — so a field for it could only ever be empty, and a scan keyed on
// it silently reconstructs nothing (#138).
type liveBlock struct {
	id             string
	typ            string
	childPageTitle string
	text           string
}

// listLiveBlocks drains the paginated GET /blocks/{id}/children for one page,
// projecting each block to a liveBlock. It recurses into a table's own children
// (its table_row blocks, whose cell text no top-level walk would otherwise see) so
// a table's live content is fingerprinted; the extra round-trip is spent only on
// pages that actually hold a table, keeping the walk O(nodes + tables). It does not
// recurse into child_page ids — the caller does that for cluster subpages, which
// are their own nodes.
func (b *Backend) listLiveBlocks(ctx context.Context, pageID string) ([]liveBlock, error) {
	var out []liveBlock
	cursor := ""
	for {
		path := "/blocks/" + url.PathEscape(pageID) + "/children?page_size=100"
		if cursor != "" {
			path += "&start_cursor=" + url.QueryEscape(cursor)
		}
		var page rawBlockList
		if err := b.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		for _, raw := range page.Results {
			lb, err := parseLiveBlock(raw)
			if err != nil {
				return nil, err
			}
			out = append(out, lb)
			// A table's rows are its children (never flat siblings), so pull them in
			// right after the table line. table_row blocks hold no tables, so this
			// recurses exactly one level.
			if lb.typ == nTypeTable && lb.id != "" {
				rows, err := b.listLiveBlocks(ctx, lb.id)
				if err != nil {
					return nil, err
				}
				out = append(out, rows...)
			}
		}
		if !page.HasMore || page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// rawBlockList is a page of GET /blocks/{id}/children, kept as raw messages so each
// block's type-keyed rich-text payload can be decoded against its own type.
type rawBlockList struct {
	Results    []json.RawMessage `json:"results"`
	NextCursor string            `json:"next_cursor"`
	HasMore    bool              `json:"has_more"`
}

// blockEnvelope is the type-agnostic head of any Notion block.
type blockEnvelope struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	ChildPage *struct {
		Title string `json:"title"`
	} `json:"child_page"`
}

// annotatedRun is one rich-text span ScanRecompute reads: its type (to fold a
// page mention to the ref placeholder, matching the source side) and its plain text
// with any external link URL, both folded into the content fingerprint.
type annotatedRun struct {
	Type      string `json:"type"`
	PlainText string `json:"plain_text"`
	Text      struct {
		Content string `json:"content"`
		Link    *struct {
			URL string `json:"url"`
		} `json:"link"`
	} `json:"text"`
}

// richTextHolder is the `{ "rich_text": [...] }` shape every text-bearing Notion
// block nests under its type key.
type richTextHolder struct {
	RichText []annotatedRun `json:"rich_text"`
}

// tableRowHolder is the `{ "cells": [[run…]…] }` shape a table_row block nests
// under its type key: an ordered row of cells, each an ordered rich-text run.
type tableRowHolder struct {
	Cells [][]annotatedRun `json:"cells"`
}

// parseLiveBlock decodes one raw block: its envelope, then — for a text-bearing
// block — the rich_text under its type key, from which it derives the block's plain
// text. A child_page block carries its title instead.
func parseLiveBlock(raw json.RawMessage) (liveBlock, error) {
	var env blockEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return liveBlock{}, fmt.Errorf("notion: scan: decode block: %w", err)
	}
	lb := liveBlock{id: env.ID, typ: env.Type}
	if env.Type == nTypeChildPage {
		if env.ChildPage != nil {
			lb.childPageTitle = env.ChildPage.Title
		}
		return lb, nil
	}

	// Pull the type-keyed object and read its content. A table_row carries cells
	// rather than a flat rich_text run; every other text-bearing block carries
	// rich_text (absent for blocks that carry none, e.g. a divider — an empty run).
	var byType map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byType); err != nil {
		return liveBlock{}, fmt.Errorf("notion: scan: decode block body: %w", err)
	}
	if body, ok := byType[env.Type]; ok {
		if env.Type == nTypeTableRow {
			var holder tableRowHolder
			if err := json.Unmarshal(body, &holder); err == nil {
				cells := make([]string, len(holder.Cells))
				for i, cell := range holder.Cells {
					cells[i] = liveCanonRunsText(cell)
				}
				lb.text = strings.Join(cells, canonCellSep)
			}
		} else {
			var holder richTextHolder
			if err := json.Unmarshal(body, &holder); err == nil {
				lb.text = liveCanonRunsText(holder.RichText)
			}
		}
	}
	return lb, nil
}

// liveCanonRunsText extracts a live rich-text run slice's canonical text, the
// mirror of the source side's canonRunsText: a page mention folds to the ref
// placeholder, every other run contributes its plain text (preferring the
// server-filled plain_text) plus any external link URL.
func liveCanonRunsText(runs []annotatedRun) string {
	var s strings.Builder
	for _, r := range runs {
		if r.Type == nTypeMention {
			s.WriteString(canonRefPlaceholder)
			continue
		}
		s.WriteString(runText(r))
		if r.Text.Link != nil && r.Text.Link.URL != "" {
			s.WriteString(canonLinkSep)
			s.WriteString(r.Text.Link.URL)
		}
	}
	return s.String()
}

func runText(r annotatedRun) string {
	if r.PlainText != "" {
		return r.PlainText
	}
	return r.Text.Content
}

// liveContentHash fingerprints a node's live content over the ordered,
// non-child_page blocks, through the same canonical serializer the source side
// runs (recompute_hash.go). child_page blocks are excluded (they are their own
// nodes); a table's rows ride as their own table_row liveBlocks (listLiveBlocks
// recurses), so cell text is covered. It is the true-drift signal — a live edit
// changes the text and thus the hash — and, aligned with sourceContentHash, lets
// an unchanged node hash-skip under --recompute (#110).
func liveContentHash(blocks []liveBlock) publish.Hash {
	var cbs []canonBlock
	for _, blk := range blocks {
		if blk.childPageTitle != "" {
			continue
		}
		cbs = append(cbs, canonBlock{typ: blk.typ, text: blk.text})
	}
	return hashCanonBlocks(cbs)
}
