package graph

import (
	"testing"
)

func onlyTable(t *testing.T, blocks []BlockContent) BlockContent {
	t.Helper()
	for _, bc := range blocks {
		if bc.Kind == Table {
			return bc
		}
	}
	t.Fatalf("no table block among %d blocks", len(blocks))
	return BlockContent{}
}

func cellText(c TableCell) string {
	var s string
	for _, in := range c.Inlines {
		s += in.Text
	}
	return s
}

// A GFM table parses into a Table block — a header row plus body rows, each cell
// carrying its own inline text — rather than falling through to a Generic/paragraph.
func TestTableParsesToRowsAndCells(t *testing.T) {
	body := "| Name | Role |\n| --- | --- |\n| Ada | author |\n| Bob | editor |\n"
	tbl := onlyTable(t, blocksFromBody(t, body))

	if !tbl.HasColumnHeader {
		t.Errorf("HasColumnHeader = false, want true for a GFM table")
	}
	if len(tbl.Rows) != 3 {
		t.Fatalf("got %d rows, want 3 (1 header + 2 body)", len(tbl.Rows))
	}
	want := [][]string{{"Name", "Role"}, {"Ada", "author"}, {"Bob", "editor"}}
	for i, row := range tbl.Rows {
		if len(row.Cells) != 2 {
			t.Fatalf("row %d: got %d cells, want 2", i, len(row.Cells))
		}
		for j, cell := range row.Cells {
			if got := cellText(cell); got != want[i][j] {
				t.Errorf("cell [%d][%d] = %q, want %q", i, j, got, want[i][j])
			}
		}
	}
}

// A ragged body row (fewer cells than the header) is normalized to the header width
// with an empty trailing cell, so every row is rectangular for the backends.
func TestTableShortRowPaddedToWidth(t *testing.T) {
	// goldmark renders body rows against the header column count; a two-column header
	// with a one-cell body row yields a padded empty second cell.
	body := "| A | B |\n| --- | --- |\n| x |\n"
	tbl := onlyTable(t, blocksFromBody(t, body))
	if len(tbl.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(tbl.Rows))
	}
	last := tbl.Rows[1]
	if len(last.Cells) != 2 {
		t.Fatalf("body row got %d cells, want 2 (padded to header width)", len(last.Cells))
	}
	if got := cellText(last.Cells[1]); got != "" {
		t.Errorf("padded cell = %q, want empty", got)
	}
}

// A concept link inside a table cell survives as a first-class Ref, and a link in a
// following paragraph still resolves — proving the cell link consumed exactly one
// link ordinal, keeping linkIdx in lockstep with the bundle's resolution just as the
// flat blocks do. If the cell's ordinal were miscounted, node:b.md would misresolve.
func TestTableCellLinkBecomesRefAndKeepsOrdinalAligned(t *testing.T) {
	files := map[string]string{
		"okf.toml": "",
		"index.md": "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"a.md":     "---\ntype: c\n---\nA.\n",
		"b.md":     "---\ntype: c\n---\nB.\n",
		"p.md":     "---\ntype: c\n---\n| Doc |\n| --- |\n| [A](a.md) |\n\nThen see [B](b.md).\n",
	}
	b := loadBundle(t, files)
	cs := seed{unchanged: []string{"index.md", "a.md", "b.md"}}.build(t, b)
	g := gen(t, b, cs)
	sc := opFor(g, nodeRef("p.md"), SetContent)
	if !hasRef(sc.Refs, nodeRef("a.md")) || !hasRef(sc.Refs, nodeRef("b.md")) {
		t.Errorf("p refs = %v, want both node:a.md (in the table cell) and node:b.md", sc.Refs)
	}
	// And the table itself is a Table block, not a paragraph fallback.
	var sawTable bool
	for _, blk := range sc.Doc.Blocks {
		if bc, ok := blk.Content.(BlockContent); ok && bc.Kind == Table {
			sawTable = true
		}
	}
	if !sawTable {
		t.Errorf("SetContent has no Table block; the table fell back to a paragraph")
	}
}
