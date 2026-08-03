package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/sigma/okf-tools/internal/areas"
	"github.com/sigma/okf-tools/internal/bundle"
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
			continue // a stray hand-made row the pipeline never created
		}
		if err := claim(path, row.ID); err != nil {
			return nil, err
		}
		node := nodeSym(path)
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
		for _, blk := range blocks {
			if blk.typ != nTypeChildPage {
				continue
			}
			subpath, ok := byID[blk.id]
			if !ok {
				subpath, ok = byTitle[blk.childPageTitle]
			}
			if !ok {
				continue
			}
			if err := claim(subpath, "subtree of "+row.ID); err != nil {
				return nil, err
			}
			sub := nodeSym(subpath)
			nodeIDs[sub] = publish.BackendID(blk.id) // live id, healing a stale stored one
			subBlocks, err := b.listLiveBlocks(ctx, blk.id)
			if err != nil {
				return nil, err
			}
			hashes[sub] = liveContentHash(subBlocks)
			// The subpage's property hash rides its stored subtree entry (stored-
			// readback), even as its content hash is reconstructed live.
			if e, ok := stored[subpath]; ok && e.PropHash != "" {
				propHashes[sub] = publish.Hash(e.PropHash)
			}
			delete(stored, subpath)
		}
		for subpath, e := range stored {
			if err := claim(subpath, "subtree of "+row.ID); err != nil {
				return nil, err
			}
			sub := nodeSym(subpath)
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

		// Anchors: a row that carries an anchors column is the glossary-role row;
		// re-derive its anchor map from the live blocks (bold-lead terms), healing a
		// stale block id. The anchor name uses the shared bundle.Slug so it matches
		// exactly what Generation minted from source.
		if plainText(row.Properties["anchors"]) != "" {
			for _, blk := range blocks {
				if blk.boldLead == "" {
					continue
				}
				name := publish.AnchorName(areas.RoleGlossary + "/" + bundle.Slug(blk.boldLead))
				anchorIDs[name] = publish.BackendID(blk.id)
			}
		}
	}

	return publish.NewCurrentStateWithProps(nodeIDs, hashes, propHashes, anchorIDs), nil
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

// liveBlock is the sliver of a live Notion block ScanRecompute needs: its id and
// type, whether it is a child_page (and that page's title, the subpath key), the
// concatenated plain text of its rich_text, and the leading bold run's text (the
// glossary anchor term, "" when the block does not lead with bold).
type liveBlock struct {
	id             string
	typ            string
	childPageTitle string
	text           string
	boldLead       string
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
// page mention to the ref placeholder, matching the source side), its plain text
// and any external link URL (folded into the content fingerprint), and the one
// annotation the glossary self-heal needs (bold, to spot an anchor's leading term).
type annotatedRun struct {
	Type      string `json:"type"`
	PlainText string `json:"plain_text"`
	Text      struct {
		Content string `json:"content"`
		Link    *struct {
			URL string `json:"url"`
		} `json:"link"`
	} `json:"text"`
	Annotations struct {
		Bold bool `json:"bold"`
	} `json:"annotations"`
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
// text and any leading bold term. A child_page block carries its title instead.
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
				if len(holder.RichText) > 0 && holder.RichText[0].Annotations.Bold {
					lb.boldLead = runText(holder.RichText[0])
				}
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
