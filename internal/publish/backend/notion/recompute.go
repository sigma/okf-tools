package notion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

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
		// True drift: hash the live content of this row's own blocks.
		hashes[node] = liveContentHash(blocks)

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
			if blk.typ != "child_page" {
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

	return publish.NewCurrentState(nodeIDs, hashes, anchorIDs), nil
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
// projecting each block to a liveBlock. It does not recurse on its own; the caller
// recurses into child_page ids it cares about (cluster subpages), so the walk cost
// stays O(nodes) round-trips as the design bounds it.
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

// annotatedRun is one rich-text span with the single annotation ScanRecompute
// reads (bold, to spot a glossary anchor's leading term).
type annotatedRun struct {
	PlainText string `json:"plain_text"`
	Text      struct {
		Content string `json:"content"`
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

// parseLiveBlock decodes one raw block: its envelope, then — for a text-bearing
// block — the rich_text under its type key, from which it derives the block's plain
// text and any leading bold term. A child_page block carries its title instead.
func parseLiveBlock(raw json.RawMessage) (liveBlock, error) {
	var env blockEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return liveBlock{}, fmt.Errorf("notion: scan: decode block: %w", err)
	}
	lb := liveBlock{id: env.ID, typ: env.Type}
	if env.Type == "child_page" {
		if env.ChildPage != nil {
			lb.childPageTitle = env.ChildPage.Title
		}
		return lb, nil
	}

	// Pull the type-keyed object and read its rich_text (absent for blocks that
	// carry none, e.g. a divider — those contribute an empty text run).
	var byType map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byType); err != nil {
		return liveBlock{}, fmt.Errorf("notion: scan: decode block body: %w", err)
	}
	if body, ok := byType[env.Type]; ok {
		var holder richTextHolder
		if err := json.Unmarshal(body, &holder); err == nil {
			lb.text = liveRunsText(holder.RichText)
			if len(holder.RichText) > 0 && holder.RichText[0].Annotations.Bold {
				lb.boldLead = runText(holder.RichText[0])
			}
		}
	}
	return lb, nil
}

// liveRunsText concatenates the plain text of a live rich-text run slice,
// preferring the server-provided plain_text and falling back to the authored
// content.
func liveRunsText(runs []annotatedRun) string {
	var s string
	for _, r := range runs {
		s += runText(r)
	}
	return s
}

func runText(r annotatedRun) string {
	if r.PlainText != "" {
		return r.PlainText
	}
	return r.Text.Content
}

// liveContentHash fingerprints a node's live content: a SHA-256 over the ordered,
// non-child_page blocks' type and text. child_page blocks are excluded (they are
// their own nodes, hashed separately), so a page's hash reflects only its own body.
// It is the true-drift signal — a live edit changes the text and thus the hash.
func liveContentHash(blocks []liveBlock) publish.Hash {
	h := sha256.New()
	for _, blk := range blocks {
		if blk.childPageTitle != "" {
			continue
		}
		fmt.Fprintf(h, "%s:%s\n", blk.typ, blk.text)
	}
	return publish.Hash(hex.EncodeToString(h.Sum(nil)))
}
