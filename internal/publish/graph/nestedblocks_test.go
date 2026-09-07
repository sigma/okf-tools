package graph

import (
	"strings"
	"testing"
)

// kindsOf names the block stream, so a failure reads as a shape rather than a
// count.
func kindsOf(blocks []BlockContent) []string {
	out := make([]string, 0, len(blocks))
	for _, bc := range blocks {
		out = append(out, bc.Kind.String())
	}
	return out
}

// nestedTableBody is the reported shape (#182): an enumerated argument with its
// evidence table indented underneath, which is a natural way to attach a table to
// a numbered point.
const nestedTableBody = "8. **Retention must be bounded.** The two models are not\n" +
	"   reconcilable by adjusting a number:\n\n" +
	"   | | Draft 2 | Here |\n" +
	"   |---|---|---|\n" +
	"   | Raw messages | 90 days | Run-scoped |\n" +
	"   | Transcripts | implied | Run-scoped |\n"

// TestTableNestedInAListItemStaysATable is the #182 regression. A table indented
// inside a list item was reached as INLINE content of the item, so its cells were
// collected as bare text runs: every separator was lost, and the item's own
// paragraph was glued to the first cell.
func TestTableNestedInAListItemStaysATable(t *testing.T) {
	blocks := blocksFromBody(t, nestedTableBody)
	tbl := onlyTable(t, blocks)

	if len(tbl.Rows) != 3 {
		t.Fatalf("got %d rows, want 3 (1 header + 2 body); blocks: %v", len(tbl.Rows), kindsOf(blocks))
	}
	want := [][]string{
		{"", "Draft 2", "Here"},
		{"Raw messages", "90 days", "Run-scoped"},
		{"Transcripts", "implied", "Run-scoped"},
	}
	for i, row := range tbl.Rows {
		for j, cell := range row.Cells {
			if got := cellText(cell); got != want[i][j] {
				t.Errorf("cell [%d][%d] = %q, want %q", i, j, got, want[i][j])
			}
		}
	}

	// The item's own text is its own block, and stops where the table starts: the
	// report shows "number:" glued to "Draft", so the block boundary was lost too.
	var item BlockContent
	for _, bc := range blocks {
		if bc.Kind == ListItem {
			item = bc
		}
	}
	text := inlineText(item)
	if !strings.Contains(text, "not reconcilable by adjusting a number:") {
		t.Errorf("the list item lost its own text: %q", text)
	}
	if strings.Contains(text, "Draft 2") {
		t.Errorf("the table's cells were flattened into the list item: %q", text)
	}
}

// TestBlocksNestedInAListItemEmitAsBlocks: the fix generalises past tables, which
// is what the report asked to confirm — every block construct that can be
// indented under a bullet was reaching the same inline path.
func TestBlocksNestedInAListItemEmitAsBlocks(t *testing.T) {
	cases := []struct {
		name, body string
		want       BlockKind
		wantText   string
	}{
		{
			name:     "fenced code",
			body:     "- a bullet:\n\n  ```yaml\n  foo: bar\n  ```\n",
			want:     CodeBlock,
			wantText: "foo: bar",
		},
		{
			name:     "blockquote",
			body:     "- a bullet:\n\n  > quoted evidence\n",
			want:     Quote,
			wantText: "quoted evidence",
		},
		{
			name:     "second paragraph",
			body:     "- a bullet:\n\n  a second paragraph.\n",
			want:     Paragraph,
			wantText: "a second paragraph.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks := blocksFromBody(t, tc.body)
			var found *BlockContent
			for i, bc := range blocks {
				if bc.Kind == tc.want {
					found = &blocks[i]
				}
			}
			if found == nil {
				t.Fatalf("no %s block; got %v", tc.want, kindsOf(blocks))
			}
			if got := inlineText(*found); !strings.Contains(got, tc.wantText) {
				t.Errorf("%s block text = %q, want it to contain %q", tc.want, got, tc.wantText)
			}
			// And the item keeps its own text without the nested block's.
			for _, bc := range blocks {
				if bc.Kind != ListItem {
					continue
				}
				if got := inlineText(bc); strings.Contains(got, tc.wantText) {
					t.Errorf("the nested %s was flattened into the list item: %q", tc.want, got)
				}
			}
		})
	}
}

// A nested list is still a nested list: it emits as deeper Item blocks, which is
// the behaviour the item recursion already had.
func TestNestedListStillEmitsDeeperItems(t *testing.T) {
	blocks := blocksFromBody(t, "- outer\n  - inner\n")
	var levels []int
	for _, bc := range blocks {
		if bc.Kind == ListItem {
			levels = append(levels, bc.Level)
		}
	}
	if len(levels) != 2 || levels[0] != 1 || levels[1] != 2 {
		t.Errorf("item levels = %v, want [1 2]; blocks: %v", levels, kindsOf(blocks))
	}
}

// TestTableNestedInAQuoteStaysATable: the report asked whether the same is true
// of "a nested table inside a blockquote". It was — a quote collects its content
// into one inline run, so a table inside it lost every separator exactly as one
// inside a list item did.
func TestTableNestedInAQuoteStaysATable(t *testing.T) {
	blocks := blocksFromBody(t, "> intro line\n>\n> | A | B |\n> |---|---|\n> | x | y |\n")
	tbl := onlyTable(t, blocks)
	if len(tbl.Rows) != 2 {
		t.Fatalf("got %d rows, want 2; blocks: %v", len(tbl.Rows), kindsOf(blocks))
	}
	if got := cellText(tbl.Rows[1].Cells[0]); got != "x" {
		t.Errorf("cell [1][0] = %q, want %q", got, "x")
	}
	for _, bc := range blocks {
		if bc.Kind != Quote {
			continue
		}
		if got := inlineText(bc); got != "intro line" {
			t.Errorf("quote text = %q, want just its own prose", got)
		}
	}
}

// A fenced block inside a quote has structure to lose too: its language and its
// literal body.
func TestCodeNestedInAQuoteStaysCode(t *testing.T) {
	blocks := blocksFromBody(t, "> intro line\n>\n> ```yaml\n> foo: bar\n> ```\n")
	code := onlyCodeBlock(t, blocks)
	if code.Language != "yaml" {
		t.Errorf("Language = %q, want yaml", code.Language)
	}
	if got := inlineText(code); !strings.Contains(got, "foo: bar") {
		t.Errorf("code text = %q", got)
	}
}

// But a quote's PROSE still flattens into one run: that is the Quote kind's
// documented shape, and only structure that cannot survive flattening is lifted
// out of it.
func TestQuoteProseStillFlattensIntoOneBlock(t *testing.T) {
	blocks := blocksFromBody(t, "> first paragraph\n>\n> second paragraph\n")
	var quotes int
	for _, bc := range blocks {
		if bc.Kind == Quote {
			quotes++
			// Folded together, but SEPARATED: two source paragraphs run into one
			// string is the same glue #181 was about.
			if got := inlineText(bc); got != "first paragraph\nsecond paragraph" {
				t.Errorf("quote text = %q, want both paragraphs with a break between them", got)
			}
		}
	}
	if quotes != 1 {
		t.Errorf("got %d quote blocks, want 1; blocks: %v", quotes, kindsOf(blocks))
	}
}

// TestQuoteKeepsDocumentOrder: lifting a table out of a quote must not hoist the
// prose that FOLLOWED it above the table. Flattening the quote first and lifting
// afterwards reads as a fix and loses the argument the quote was making.
func TestQuoteKeepsDocumentOrder(t *testing.T) {
	blocks := blocksFromBody(t,
		"> before the table\n>\n> | A | B |\n> |---|---|\n> | x | y |\n>\n> after the table\n")
	kinds := kindsOf(blocks)
	if len(kinds) != 3 || kinds[0] != "quote" || kinds[1] != "table" || kinds[2] != "quote" {
		t.Fatalf("block kinds = %v, want [quote table quote]", kinds)
	}
	if got := inlineText(blocks[0]); got != "before the table" {
		t.Errorf("first quote = %q", got)
	}
	if got := inlineText(blocks[2]); got != "after the table" {
		t.Errorf("second quote = %q", got)
	}
}

// TestNestingSurvivesMoreThanOneLevel: a quote inside a quote, or a quote inside
// a bullet, is itself a container — so a table one level further down must not
// collapse back into running text.
func TestNestingSurvivesMoreThanOneLevel(t *testing.T) {
	cases := []struct{ name, body string }{
		{"quote in quote", "> outer\n>\n> > | A | B |\n> > |---|---|\n> > | x | y |\n"},
		{"quote under a bullet", "- bullet:\n\n  > | A | B |\n  > |---|---|\n  > | x | y |\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks := blocksFromBody(t, tc.body)
			tbl := onlyTable(t, blocks)
			if len(tbl.Rows) != 2 {
				t.Fatalf("got %d rows, want 2; blocks: %v", len(tbl.Rows), kindsOf(blocks))
			}
			if got := cellText(tbl.Rows[1].Cells[0]); got != "x" {
				t.Errorf("cell [1][0] = %q, want %q", got, "x")
			}
		})
	}
}

// A bullet whose whole content is a table has no text of its own, and an empty
// bullet beside the table is noise rather than structure — the table is what the
// item holds.
func TestAnItemThatIsOnlyATableEmitsNoEmptyBullet(t *testing.T) {
	blocks := blocksFromBody(t, "- | A | B |\n  |---|---|\n  | x | y |\n")
	for _, bc := range blocks {
		if bc.Kind == ListItem && len(bc.Inlines) == 0 {
			t.Errorf("an empty list item was emitted; blocks: %v", kindsOf(blocks))
		}
	}
	if len(onlyTable(t, blocks).Rows) != 2 {
		t.Errorf("the table did not survive; blocks: %v", kindsOf(blocks))
	}
}
