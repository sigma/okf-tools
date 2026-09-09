package notion

import (
	"context"
	"fmt"
	"net/http"
	"strings"
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

// bigTableRows builds n single-cell rows labelled r0…r(n-1), the shape a long
// mirrored table has: many rows, each trivially small.
func bigTableRows(n int) []tableRow {
	rows := make([]tableRow, 0, n)
	for i := range n {
		rows = append(rows, tableRow{cells: [][]publish.Run{{{Text: fmt.Sprintf("r%d", i)}}}})
	}
	return rows
}

// rowAppends returns the PATCH /blocks/{id}/children requests, in order, that
// carried table_row children — the overflow appends, told apart from a page's own
// content append by what they wrote.
func rowAppends(f *fakeNotion) []recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedReq
	for _, req := range f.reqs {
		if req.Method != http.MethodPatch || !strings.HasPrefix(req.Path, "/blocks/") || !strings.HasSuffix(req.Path, "/children") {
			continue
		}
		children, _ := req.Body["children"].([]any)
		if len(children) == 0 {
			continue
		}
		if first, ok := children[0].(map[string]any); ok && first["type"] == nTypeTableRow {
			out = append(out, req)
		}
	}
	return out
}

// rowTexts extracts each row's first-cell text from a children array, so a test can
// assert WHICH rows a call carried and in what order.
func rowTexts(t *testing.T, children []any) []string {
	t.Helper()
	var out []string
	for _, raw := range children {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("table row is %T, want an object", raw)
		}
		tr := digInto(t, row, nTypeTableRow)
		cells, _ := tr["cells"].([]any)
		cell0, _ := cells[0].([]any)
		rt, _ := cell0[0].(map[string]any)
		out = append(out, digInto(t, rt, "text")["content"].(string))
	}
	return out
}

// A table longer than Notion's ≤100-children ceiling publishes as ONE table: the
// create carries the first 100 rows inline, and the rest are appended onto the table
// block itself, in order. Before sigma/okf-tools#207 every row rode in the create,
// and Notion rejected the whole POST — the table's rows are its children, so the
// ceiling applies to them too, which the Bin (one unit of Cost 1 per table) cannot
// see. The fake enforces that ceiling, so this fails without the fix.
func TestExecuteOversizedTableAppendsOverflowRows(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:a.md", Node: "node:a.md", Create: true,
		Children: []childBlock{paraBlock("intro"), {
			kind:            int(graph.Table),
			hasColumnHeader: true,
			rows:            bigTableRows(134),
		}},
	}
	if _, err := be.Execute(context.Background(), txn, stubResolver{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	body := f.requestsTo(http.MethodPost, "/pages")[0].Body
	children, _ := body["children"].([]any)
	block, _ := children[1].(map[string]any)
	inline, _ := digInto(t, block, "table")["children"].([]any)
	if len(inline) != maxBlocksPerTxn {
		t.Fatalf("create carried %d table rows inline, want %d", len(inline), maxBlocksPerTxn)
	}
	if got := rowTexts(t, inline); got[0] != "r0" || got[len(got)-1] != "r99" {
		t.Errorf("inline rows run %s…%s, want r0…r99", got[0], got[len(got)-1])
	}

	appends := rowAppends(f)
	if len(appends) != 1 {
		t.Fatalf("got %d table-row appends, want 1", len(appends))
	}
	// The rows land on the TABLE block, not the page: they are the table's children.
	tableID, _ := f.blocks["page-1"][1]["id"].(string)
	if want := "/blocks/" + tableID + "/children"; appends[0].Path != want {
		t.Errorf("overflow rows appended to %s, want the table block at %s", appends[0].Path, want)
	}
	rest, _ := appends[0].Body["children"].([]any)
	got := rowTexts(t, rest)
	if len(got) != 34 || got[0] != "r100" || got[33] != "r133" {
		t.Errorf("appended rows = %d rows %v…%v, want 34 rows r100…r133", len(got), got[0], got[len(got)-1])
	}
}

// The overflow itself is batched against the same ceiling: a table far over it takes
// as many appends as it needs, each within the cap and in row order, rather than one
// oversized append that would 400 exactly as the create did.
func TestExecuteOversizedTableBatchesOverflowAppends(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:a.md", Node: "node:a.md", Create: true,
		Children: []childBlock{{kind: int(graph.Table), hasColumnHeader: true, rows: bigTableRows(250)}},
	}
	if _, err := be.Execute(context.Background(), txn, stubResolver{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	appends := rowAppends(f)
	if len(appends) != 2 {
		t.Fatalf("got %d table-row appends for 250 rows, want 2", len(appends))
	}
	var seen []string
	for _, req := range appends {
		rows, _ := req.Body["children"].([]any)
		if len(rows) > maxBlocksPerTxn {
			t.Fatalf("an overflow append carried %d rows, over the %d ceiling", len(rows), maxBlocksPerTxn)
		}
		seen = append(seen, rowTexts(t, rows)...)
	}
	if len(seen) != 150 || seen[0] != "r100" || seen[149] != "r249" {
		t.Errorf("appended rows = %d rows %v…%v, want 150 rows r100…r249", len(seen), seen[0], seen[len(seen)-1])
	}
}

// The append path splits an oversized table too, not just the create: a table can
// land in a follow-on content append (an overflow chunk, or a node whose page
// already exists), and there the table block's id comes back in the append's own
// response rather than from a follow-up GET.
func TestExecuteOversizedTableInAppendPath(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:a.md", Node: "node:a.md", AssertsContent: true,
		Children: []childBlock{{kind: int(graph.Table), hasColumnHeader: true, rows: bigTableRows(134)}},
	}
	r := stubResolver{"node:a.md": "page-existing"}
	if _, err := be.Execute(context.Background(), txn, r); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	content := f.requestsTo(http.MethodPatch, "/blocks/page-existing/children")
	if len(content) != 1 {
		t.Fatalf("got %d content appends, want 1", len(content))
	}
	children, _ := content[0].Body["children"].([]any)
	block, _ := children[0].(map[string]any)
	inline, _ := digInto(t, block, "table")["children"].([]any)
	if len(inline) != maxBlocksPerTxn {
		t.Fatalf("content append carried %d table rows inline, want %d", len(inline), maxBlocksPerTxn)
	}

	appends := rowAppends(f)
	if len(appends) != 1 {
		t.Fatalf("got %d table-row appends, want 1", len(appends))
	}
	tableID, _ := f.blocks["page-existing"][0]["id"].(string)
	if want := "/blocks/" + tableID + "/children"; appends[0].Path != want {
		t.Errorf("overflow rows appended to %s, want the table block at %s", appends[0].Path, want)
	}
	rest, _ := appends[0].Body["children"].([]any)
	if got := rowTexts(t, rest); len(got) != 34 || got[0] != "r100" {
		t.Errorf("appended rows = %d rows from %v, want 34 rows from r100", len(got), got[0])
	}
}

// A table within the ceiling still publishes in one call — the fix must not spend a
// round-trip per table on the common case.
func TestExecuteSmallTableMakesNoExtraCall(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:a.md", Node: "node:a.md", Create: true,
		Children: []childBlock{{kind: int(graph.Table), rows: bigTableRows(maxBlocksPerTxn)}},
	}
	if _, err := be.Execute(context.Background(), txn, stubResolver{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := len(rowAppends(f)); n != 0 {
		t.Errorf("a %d-row table took %d overflow appends, want 0", maxBlocksPerTxn, n)
	}
	if n := f.countPath(http.MethodGet, "/blocks/page-1/children"); n != 0 {
		t.Errorf("a table within the ceiling listed the page's children %d times, want 0", n)
	}
}
