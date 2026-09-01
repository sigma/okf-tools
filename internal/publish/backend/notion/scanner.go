package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// Scan produces the neutral CurrentState, dispatching on the producer-side mode
// (#167 decision 1). Both modes return the identical neutral CurrentState, so the
// #163 consumer seam is untouched; only the reconstruction behind it differs:
//
//   - ScanStored (steady-state default): one paginated data-source query over the
//     self-describing derived columns, zero per-page block reads (scanStored).
//   - ScanRecompute (opt-in): the full live block walk that recomputes ContentHash
//     from live content and re-derives subpage ids and the anchor map, self-healing
//     staleness the cheap path cannot see (scanRecompute, recompute.go).
func (b *Backend) Scan(ctx context.Context, mode backend.ScanMode) (*publish.CurrentState, error) {
	if mode == backend.ScanRecompute {
		return b.scanRecompute(ctx)
	}
	return b.scanStored(ctx)
}

// scanStored is the cheap steady-state producer: a single paginated data-source
// query over self-describing derived columns, with zero per-page block reads.
// Every top-level row carries the columns that seed the whole snapshot — `path`
// (the node's repo path → its SymbolicID), `hash` (its content hash), `hashes`
// (the #146 subtree map of `subpath → {id, hash}` for cluster subpages), and, on
// the glossary-role row, `anchors` (`anchor-name → block id`). So one query
// resolves NodeID, ContentHash, AnchorID, and Nodes() with no follow-on reads.
func (b *Backend) scanStored(ctx context.Context) (*publish.CurrentState, error) {
	rows, err := b.queryAllRows(ctx)
	if err != nil {
		return nil, err
	}

	nodeIDs := map[publish.SymbolicID]publish.BackendID{}
	hashes := map[publish.SymbolicID]publish.Hash{}
	propHashes := map[publish.SymbolicID]publish.Hash{}
	anchorIDs := map[publish.AnchorName]publish.BackendID{}
	claim := newPathClaimer()

	for _, row := range rows {
		path := plainText(row.Properties["path"])
		if path == "" {
			// A row carrying no path is either a stray the pipeline never created or —
			// far more likely — one IT created and could not record before the run died
			// (#135). The two are indistinguishable from here, and both are unusable:
			// nothing can key them to a source node. Surface them as unclaimed so
			// reconciliation reclaims them instead of leaking one per aborted run.
			markUnclaimed(nodeIDs, row.ID)
			continue
		}
		if err := claim(path, row.ID); err != nil {
			return nil, err
		}
		sym := publish.NodeRef(path)
		nodeIDs[sym] = publish.BackendID(row.ID)
		if h := plainText(row.Properties["hash"]); h != "" {
			// The `hash` column stores content and property hashes as one compound
			// value (the two-hash split, #110 phase 2); split it into both tables so
			// SetContent and SetProperties hash-skip independently.
			content, prop := decodeHashPair(h)
			hashes[sym] = content
			propHashes[sym] = prop
		}

		if err := readSubtree(plainText(row.Properties["hashes"]), row.ID, claim, nodeIDs, hashes, propHashes); err != nil {
			return nil, err
		}
		if err := readAnchors(plainText(row.Properties["anchors"]), anchorIDs); err != nil {
			return nil, err
		}
	}

	return publish.NewCurrentStateWithProps(nodeIDs, hashes, propHashes, anchorIDs), nil
}

// queryAllRows drains the paginated POST /data_sources/{id}/query, returning every
// owned top-level row. This is the single data-source query the ScanStored path
// spends — O(rows/100) round-trips, no per-page reads.
func (b *Backend) queryAllRows(ctx context.Context) ([]queryRow, error) {
	var rows []queryRow
	cursor := ""
	path := "/data_sources/" + b.dataSourceID + "/query"
	for {
		var page queryResp
		if err := b.do(ctx, http.MethodPost, path, queryReq{StartCursor: cursor, PageSize: 100}, &page); err != nil {
			return nil, err
		}
		rows = append(rows, page.Results...)
		if !page.HasMore || page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return rows, nil
}

// subtreeEntry is one cluster subpage's self-description in the #146 subtree map:
// its real Notion page id and content hash (so NodeID and ContentHash resolve for a
// subpage the top-level query never returns as its own row) plus its live page
// title. The title is what lets ScanRecompute match a live child_page back to its
// subpath when the id has gone stale (a page title is all Notion carries; the repo
// path is not recoverable from the live block otherwise), so the id can self-heal.
type subtreeEntry struct {
	ID       string `json:"id"`
	Hash     string `json:"hash"`
	PropHash string `json:"prop_hash,omitempty"`
	Title    string `json:"title,omitempty"`
}

// readSubtree folds a row's `hashes` subtree-map column into the node/hash tables:
// each subpath becomes node:<subpath> resolving to the stored id and hash, with the
// same 1:1 path-uniqueness guard as top-level rows.
func readSubtree(raw, rowID string, claim func(path, by string) error, nodeIDs map[publish.SymbolicID]publish.BackendID, hashes, propHashes map[publish.SymbolicID]publish.Hash) error {
	sub, err := storedSubtree(raw, rowID)
	if err != nil {
		return err
	}
	for subpath, e := range sub {
		if err := claim(subpath, "subtree of "+rowID); err != nil {
			return err
		}
		sym := publish.NodeRef(subpath)
		if e.ID != "" {
			nodeIDs[sym] = publish.BackendID(e.ID)
		}
		if e.Hash != "" {
			hashes[sym] = publish.Hash(e.Hash)
		}
		if e.PropHash != "" {
			propHashes[sym] = publish.Hash(e.PropHash)
		}
	}
	return nil
}

// readAnchors folds the glossary-role row's `anchors` column (anchor-name → block
// id) into the anchor table, so a page linking into an unchanged glossary resolves
// its anchors straight from the seed with no rewrite.
func readAnchors(raw string, anchorIDs map[publish.AnchorName]publish.BackendID) error {
	if raw == "" {
		return nil
	}
	var am map[string]string
	if err := json.Unmarshal([]byte(raw), &am); err != nil {
		return fmt.Errorf("notion: scan: anchors column: %w", err)
	}
	for name, id := range am {
		anchorIDs[publish.AnchorName(name)] = publish.BackendID(id)
	}
	return nil
}

// newPathClaimer returns a claim closure guarding the mirror's 1:1 path invariant:
// the first page to claim a repo path owns it; a second claim (another top-level
// row, or a subtree-map entry) is a hard error naming both pages — never a silent
// last-writer-wins over unrepairable state. Both scan modes share it.
func newPathClaimer() func(path, by string) error {
	owner := map[string]string{}
	return func(path, by string) error {
		if prev, dup := owner[path]; dup {
			return fmt.Errorf("notion: scan: path %q claimed by two pages (%s and %s)", path, prev, by)
		}
		owner[path] = by
		return nil
	}
}

// markUnclaimed records a path-less row as an unclaimed object resolving to itself,
// so reconciliation reclaims it (#135). Both scan modes share it: a row without a
// path is unusable to either, and a third scan mode must reach the same conclusion
// rather than re-deciding what an unidentifiable row means.
func markUnclaimed(nodeIDs map[publish.SymbolicID]publish.BackendID, rowID string) {
	nodeIDs[publish.UnclaimedRef(publish.BackendID(rowID))] = publish.BackendID(rowID)
}
