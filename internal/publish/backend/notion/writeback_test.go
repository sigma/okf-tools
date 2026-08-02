package notion

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
)

// TestWriteBackTopLevelRow: a top-level node's provenance is PATCHed onto its own
// row as the `path` and `hash` derived columns (and `anchors` for a glossary row),
// so the next ScanStored reads current values.
func TestWriteBackTopLevelRow(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	prov := publish.Provenance{Nodes: map[publish.SymbolicID]publish.NodeProvenance{
		"node:CONTEXT.md": {
			ID:      "page-g",
			Hash:    "hG",
			Anchors: map[publish.AnchorName]publish.BackendID{"glossary/root-kek": "block-kek"},
		},
	}}
	if err := be.WriteBack(context.Background(), prov); err != nil {
		t.Fatalf("WriteBack: %v", err)
	}

	patches := f.requestsTo("PATCH", "/pages/page-g")
	if len(patches) != 1 {
		t.Fatalf("want 1 property PATCH on the row, got %d", len(patches))
	}
	props := digInto(t, patches[0].Body, "properties")
	if got := columnText(t, props, "path"); got != "CONTEXT.md" {
		t.Errorf("path column = %q, want CONTEXT.md", got)
	}
	if got := columnText(t, props, "hash"); got != "hG" {
		t.Errorf("hash column = %q, want hG", got)
	}
	var anchors map[string]string
	if err := json.Unmarshal([]byte(columnText(t, props, "anchors")), &anchors); err != nil {
		t.Fatalf("anchors column not valid JSON: %v", err)
	}
	if anchors["glossary/root-kek"] != "block-kek" {
		t.Errorf("anchors column = %v, want the hosted anchor's block id", anchors)
	}
}

// TestWriteBackSubpageFoldsIntoParentSubtree: a subpage's {id, hash} is merged into
// its parent row's `hashes` subtree map via a read-modify-write, preserving the
// map's other members.
func TestWriteBackSubpageFoldsIntoParentSubtree(t *testing.T) {
	f := newFakeNotion()
	// The parent row already carries one other subpage in its subtree map — the
	// read-modify-write must keep it.
	f.pageProps["page-root"] = map[string]any{
		"hashes": richProp(mustJSON(t, map[string]subtreeEntry{
			"docs/adr/existing.md": {ID: "page-existing", Hash: "hExisting"},
		})),
	}
	be := newServer(t, f)

	prov := publish.Provenance{Nodes: map[publish.SymbolicID]publish.NodeProvenance{
		"node:docs/adr/new.md": {
			ID:       "page-new",
			Hash:     "hNew",
			Parent:   "node:index.md",
			ParentID: "page-root",
			Title:    "New Page",
		},
	}}
	if err := be.WriteBack(context.Background(), prov); err != nil {
		t.Fatalf("WriteBack: %v", err)
	}

	// It reads the parent's current map, then PATCHes the merged one back.
	if n := f.countPath("GET", "/pages/page-root"); n != 1 {
		t.Errorf("subtree write-back should read the parent row once, got %d", n)
	}
	patches := f.requestsTo("PATCH", "/pages/page-root")
	if len(patches) != 1 {
		t.Fatalf("want 1 subtree PATCH on the parent, got %d", len(patches))
	}
	var merged map[string]subtreeEntry
	if err := json.Unmarshal([]byte(columnText(t, digInto(t, patches[0].Body, "properties"), "hashes")), &merged); err != nil {
		t.Fatalf("hashes column not valid JSON: %v", err)
	}
	if e := merged["docs/adr/new.md"]; e.ID != "page-new" || e.Hash != "hNew" || e.Title != "New Page" {
		t.Errorf("new subpage entry = %+v, want {page-new hNew New Page}", e)
	}
	if e := merged["docs/adr/existing.md"]; e.ID != "page-existing" {
		t.Errorf("read-modify-write dropped the existing subpage entry: %+v", merged)
	}
}

// TestWriteBackEmptyIsNoOp: empty provenance (a steady-state unchanged run) writes
// nothing — the near-noop property.
func TestWriteBackEmptyIsNoOp(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)
	if err := be.WriteBack(context.Background(), publish.Provenance{}); err != nil {
		t.Fatalf("WriteBack: %v", err)
	}
	if len(f.reqs) != 0 {
		t.Errorf("empty write-back should issue no requests, got %d", len(f.reqs))
	}
}

// columnText extracts the single rich-text span content of a derived column from a
// PATCH properties body.
func columnText(t *testing.T, props map[string]any, key string) string {
	t.Helper()
	col, ok := props[key].(map[string]any)
	if !ok {
		t.Fatalf("column %q missing or not an object: %v", key, props[key])
	}
	spans, ok := col["rich_text"].([]any)
	if !ok || len(spans) == 0 {
		t.Fatalf("column %q has no rich_text spans: %v", key, col)
	}
	span, _ := spans[0].(map[string]any)
	return digInto(t, span, "text")["content"].(string)
}
