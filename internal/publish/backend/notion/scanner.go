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

	t := newScanTables()
	for _, row := range rows {
		path := plainText(row.Properties["path"])
		if path == "" {
			// A row carrying no path is either a stray the pipeline never created or —
			// far more likely — one IT created and could not record before the run died
			// (#135). The two are indistinguishable from here, and both are unusable:
			// nothing can key them to a source node. Surface them as unclaimed so
			// reconciliation reclaims them instead of leaking one per aborted run.
			t.markUnclaimed(row.ID)
			continue
		}
		sym, err := t.addRow(path, row.ID)
		if err != nil {
			return nil, err
		}
		if h := plainText(row.Properties["hash"]); h != "" {
			// The `hash` column stores content and property hashes as one compound
			// value (the two-hash split, #110 phase 2); split it into both tables so
			// SetContent and SetProperties hash-skip independently.
			content, prop := decodeHashPair(h)
			t.hashes[sym] = content
			t.propHashes[sym] = prop
		}

		if err := t.readSubtree(plainText(row.Properties["hashes"]), row.ID); err != nil {
			return nil, err
		}
		if err := readAnchors(plainText(row.Properties["anchors"]), t.anchorIDs); err != nil {
			return nil, err
		}
	}

	b.rememberRecorders(t.recordedBy)
	return t.currentState(), nil
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
	// Anchors are the anchors this subpage's content hosts, name → block id — the
	// subpage counterpart of a row's `anchors` column (sigma/okf-tools#142).
	//
	// A node's anchors have to be recorded wherever that node's record lives, and a
	// subpage's record lives here. Without it a glossary hosted as a subpage recorded
	// nothing, so every page citing it had a reference no later run could resolve:
	// the run aborted, and re-asserting the glossary — the only thing that re-mints
	// the map — never happened, because its content was unchanged. Omitted when empty,
	// so entries for ordinary subpages are unchanged on the wire.
	Anchors map[string]string `json:"anchors,omitempty"`
}

// record is where one node's self-description lives: the ROW carrying it, and
// whether that row IS the node. A top-level node is its own row and describes
// itself; a cluster subpage has no row, so it is described from an ancestor's
// `hashes` map, which outlives the subpage and has to be told when it goes
// (sigma/okf-tools#189).
type record struct {
	// subpath is the node's repo path — the key of its entry in the row's map.
	subpath string
	// row is the id of the row whose column carries the description.
	row string
	// own says the row is the node itself, so archiving the page archives the
	// description with it and there is nothing left to forget.
	own bool
}

// scanTables is the neutral snapshot a scan fills in, plus the two things that have
// to travel with it: the 1:1 path-claim guard, and the record of WHERE each node is
// described. Both scan modes fill exactly this set — the cheap one from stored
// columns, the recompute one from the live walk — so they share the shape rather
// than passing the same six values down parallel call chains.
type scanTables struct {
	nodeIDs    map[publish.SymbolicID]publish.BackendID
	hashes     map[publish.SymbolicID]publish.Hash
	propHashes map[publish.SymbolicID]publish.Hash
	anchorIDs  map[publish.AnchorName]publish.BackendID
	// recordedBy is what the scan learned about where each node's description lives.
	// It is not part of the neutral CurrentState — which knows nodes, not the columns
	// that describe them — and it cannot be re-derived later, so the scan records it
	// while it holds both halves (#189).
	recordedBy map[string]record
	// owner guards the mirror's 1:1 path invariant: the first page to claim a repo
	// path owns it; a second claim is a hard error naming both pages, never a silent
	// last-writer-wins over unrepairable state.
	owner map[string]string
}

func newScanTables() *scanTables {
	return &scanTables{
		nodeIDs:    map[publish.SymbolicID]publish.BackendID{},
		hashes:     map[publish.SymbolicID]publish.Hash{},
		propHashes: map[publish.SymbolicID]publish.Hash{},
		anchorIDs:  map[publish.AnchorName]publish.BackendID{},
		recordedBy: map[string]record{},
		owner:      map[string]string{},
	}
}

// currentState projects the filled tables into the neutral snapshot the pipeline
// consumes. What the scan learned about WHERE nodes are described stays behind,
// with the backend that has to write those columns.
func (t *scanTables) currentState() *publish.CurrentState {
	return publish.NewCurrentStateWithProps(t.nodeIDs, t.hashes, t.propHashes, t.anchorIDs)
}

// claim asserts that path is claimed by exactly one page, naming both on a clash.
func (t *scanTables) claim(path, by string) error {
	if prev, dup := t.owner[path]; dup {
		return fmt.Errorf("notion: scan: path %q claimed by two pages (%s and %s)", path, prev, by)
	}
	t.owner[path] = by
	return nil
}

// addRow claims a top-level row's path and records that the row both IS the node
// and describes it, returning the node's symbolic id.
func (t *scanTables) addRow(path, rowID string) (publish.SymbolicID, error) {
	if err := t.claim(path, rowID); err != nil {
		return "", err
	}
	sym := publish.NodeRef(path)
	t.nodeIDs[sym] = publish.BackendID(rowID)
	t.recordedBy[path] = record{subpath: path, row: rowID, own: true}
	return sym, nil
}

// markUnclaimed records a path-less row as an unclaimed object resolving to itself,
// so reconciliation reclaims it (#135). Both scan modes share it: a row without a
// path is unusable to either, and a third scan mode must reach the same conclusion
// rather than re-deciding what an unidentifiable row means.
func (t *scanTables) markUnclaimed(rowID string) {
	t.nodeIDs[publish.UnclaimedRef(publish.BackendID(rowID))] = publish.BackendID(rowID)
}

// readSubtree parses a row's `hashes` subtree-map column and folds it into the
// tables.
func (t *scanTables) readSubtree(raw, rowID string) error {
	sub, err := storedSubtree(raw, rowID)
	if err != nil {
		return err
	}
	return t.foldSubtree(sub, rowID)
}

// foldSubtree folds one row's parsed subtree map into the tables: each subpath
// becomes node:<subpath> resolving to the stored id and hash, with the same 1:1
// path-uniqueness guard as top-level rows, and recorded as described BY that row.
// Both scan modes fold through here — ScanRecompute has already parsed the map to
// drive its live walk, so it hands the parsed form straight in.
func (t *scanTables) foldSubtree(sub map[string]subtreeEntry, rowID string) error {
	for subpath, e := range sub {
		if err := t.claim(subpath, "subtree of "+rowID); err != nil {
			return err
		}
		t.recordedBy[subpath] = record{subpath: subpath, row: rowID}
		sym := publish.NodeRef(subpath)
		if e.ID != "" {
			t.nodeIDs[sym] = publish.BackendID(e.ID)
		}
		if e.Hash != "" {
			t.hashes[sym] = publish.Hash(e.Hash)
		}
		if e.PropHash != "" {
			t.propHashes[sym] = publish.Hash(e.PropHash)
		}
		foldAnchors(e.Anchors, t.anchorIDs)
	}
	return nil
}

// foldAnchors folds a recorded name → block-id map into the neutral anchor table.
// Both the row's `anchors` column and a subpage's entry (#142) arrive through here,
// so a citing page resolves an anchor identically whichever kind of node hosts it.
func foldAnchors(recorded map[string]string, anchorIDs map[publish.AnchorName]publish.BackendID) {
	for name, id := range recorded {
		anchorIDs[publish.AnchorName(name)] = publish.BackendID(id)
	}
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
