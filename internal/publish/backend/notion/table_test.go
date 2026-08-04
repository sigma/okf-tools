package notion

import (
	"context"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/graph"
)

// A table childBlock serializes to a Notion `table` block: table_width and
// has_column_header set from the block, has_row_header always false, and one
// table_row child per row carrying its cells as arrays of rich text. This is the fix
// for #106 — previously the block fell through to a paragraph of pipe text.
func TestExecuteTableEmitsTableBlock(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:a.md", Node: "node:a.md", Create: true,
		Children: []childBlock{{
			kind:            int(graph.Table),
			hasColumnHeader: true,
			rows: []tableRow{
				{cells: [][]publish.Run{{{Text: "Name"}}, {{Text: "Role"}}}},
				{cells: [][]publish.Run{{{Text: "Ada"}}, {{Text: "author"}}}},
			},
		}},
	}
	if _, err := be.Execute(context.Background(), txn, stubResolver{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	body := f.requestsTo("POST", "/pages")[0].Body
	children, _ := body["children"].([]any)
	block, _ := children[0].(map[string]any)
	if block["type"] != "table" {
		t.Fatalf("block type = %v, want table", block["type"])
	}
	table := digInto(t, block, "table")
	if table["table_width"] != float64(2) { // JSON numbers decode to float64
		t.Errorf("table_width = %v, want 2", table["table_width"])
	}
	if table["has_column_header"] != true {
		t.Errorf("has_column_header = %v, want true", table["has_column_header"])
	}
	if table["has_row_header"] != false {
		t.Errorf("has_row_header = %v, want false", table["has_row_header"])
	}
	rows, _ := table["children"].([]any)
	if len(rows) != 2 {
		t.Fatalf("got %d table_row children, want 2", len(rows))
	}
	firstRow, _ := rows[0].(map[string]any)
	if firstRow["type"] != "table_row" {
		t.Errorf("row type = %v, want table_row", firstRow["type"])
	}
	tr := digInto(t, firstRow, "table_row")
	cells, _ := tr["cells"].([]any)
	if len(cells) != 2 {
		t.Fatalf("header row has %d cells, want 2", len(cells))
	}
	// cells[0] is an array of rich-text objects; its first object's text.content is "Name".
	cell0, _ := cells[0].([]any)
	rt, _ := cell0[0].(map[string]any)
	text := digInto(t, rt, "text")
	if text["content"] != "Name" {
		t.Errorf("first cell content = %v, want Name", text["content"])
	}
}

// A Ref inside a table cell is resolved to a Notion page mention through the
// Resolver, exactly as a paragraph's inline Ref is — the cross-reference authored in
// a table survives into the mirror as a real mention, not flattened text.
func TestExecuteTableCellRefResolvesToMention(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:a.md", Node: "node:a.md", Create: true,
		Children: []childBlock{{
			kind:            int(graph.Table),
			hasColumnHeader: true,
			rows: []tableRow{
				{cells: [][]publish.Run{{{Text: "See"}}}},
				{cells: [][]publish.Run{{{Ref: "node:b.md"}}}},
			},
		}},
	}
	r := stubResolver{"node:b.md": "page-b-real"}
	if _, err := be.Execute(context.Background(), txn, r); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	body := f.requestsTo("POST", "/pages")[0].Body
	children, _ := body["children"].([]any)
	block, _ := children[0].(map[string]any)
	table := digInto(t, block, "table")
	rows, _ := table["children"].([]any)
	bodyRow, _ := rows[1].(map[string]any)
	tr := digInto(t, bodyRow, "table_row")
	cells, _ := tr["cells"].([]any)
	cell0, _ := cells[0].([]any)
	mention, _ := cell0[0].(map[string]any)
	page := digInto(t, mention, "mention", "page")
	if page["id"] != "page-b-real" {
		t.Errorf("cell Ref should resolve to the mention page id, got %v", page["id"])
	}
}
