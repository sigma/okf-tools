package graph

import (
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
)

// TestDecodeBlockAppliesTheAggregateRefsFallback pins the rule that used to be
// written out in all three tokenizers: a block whose content exposed no first-class
// inline Refs still contributes its declared Refs, so a late-bound reference in a
// degenerate content shape is not silently dropped and its dependency edge still
// forms.
func TestDecodeBlockAppliesTheAggregateRefsFallback(t *testing.T) {
	d := DecodeBlock(publish.Block{
		Content: "plain text with no inline refs",
		Refs:    []publish.SymbolicID{"node:b.md"},
	})

	if d.HadInlineRefs {
		t.Error("a bare string block exposes no inline refs")
	}
	if len(d.Refs) != 1 || d.Refs[0] != "node:b.md" {
		t.Errorf("Refs = %v, want the block's aggregate refs as the fallback", d.Refs)
	}
}

// TestDecodeBlockPrefersInlineRefs is the other half of the rule: when the content
// DID expose inline refs, those are the unit's refs and the aggregate is not piled
// on top.
func TestDecodeBlockPrefersInlineRefs(t *testing.T) {
	d := DecodeBlock(publish.Block{
		Content: BlockContent{Kind: Paragraph, Inlines: []Inline{
			{Text: "see "},
			{Text: "B", Ref: &Ref{ID: "node:b.md"}},
		}},
		Refs: []publish.SymbolicID{"node:b.md"},
	})

	if !d.HadInlineRefs {
		t.Fatal("a Ref inline must be reported as an inline ref")
	}
	if len(d.Refs) != 1 {
		t.Errorf("Refs = %v, want only the inline ref, not the aggregate as well", d.Refs)
	}
}

// TestDecodeBlockCarvesOutTables covers the shape distinction every tokenizer had
// to make for itself: a table's inline content lives per-cell, so it flattens into
// Rows rather than Runs and never chunks against a per-block cap.
func TestDecodeBlockCarvesOutTables(t *testing.T) {
	d := DecodeBlock(publish.Block{Content: BlockContent{
		Kind:            Table,
		HasColumnHeader: true,
		Rows: []TableRow{
			{Cells: []TableCell{{Inlines: []Inline{{Text: "h1"}}}}},
			{Cells: []TableCell{{Inlines: []Inline{{Text: "c1"}}}}},
		},
	}})

	if !d.IsTable || d.Kind != Table {
		t.Fatalf("IsTable = %v, Kind = %v, want a table", d.IsTable, d.Kind)
	}
	if len(d.Runs) != 0 {
		t.Errorf("a table's inline content lives per-cell; Runs = %v, want empty", d.Runs)
	}
	if len(d.Rows) != 2 {
		t.Errorf("Rows = %d, want the header row and one body row", len(d.Rows))
	}
	if !d.HasColumnHeader {
		t.Error("HasColumnHeader must survive the decode")
	}
}

// TestTokenizeOnePerBlockShape is the contract the filesystem and Docs backends now
// share: one unit per block, cost 1, the document's group on every unit, and the
// first unit asserting the node's content.
func TestTokenizeOnePerBlockShape(t *testing.T) {
	doc := publish.Document{Group: "node:a.md", Blocks: []publish.Block{
		{Content: BlockContent{Kind: Heading, Level: 1, Inlines: []Inline{{Text: "Title"}}}},
		{Content: BlockContent{Kind: Paragraph, Inlines: []Inline{{Text: "Body"}}},
			Anchors: []publish.AnchorName{"glossary/dek"}},
	}}

	units := TokenizeOnePerBlock(doc, func(d DecodedBlock) publish.BackendBlock { return d.Kind })

	if len(units) != 2 {
		t.Fatalf("units = %d, want one per block", len(units))
	}
	if !units[0].AssertsContent {
		t.Error("the first unit of a Document asserts the node's whole content")
	}
	if units[1].AssertsContent {
		t.Error("a later unit continues the assertion and must not restate it")
	}
	for i, u := range units {
		if u.Group != "node:a.md" {
			t.Errorf("unit %d group = %q, want the document's group", i, u.Group)
		}
		if c, _ := u.Cost.(int); c != 1 {
			t.Errorf("unit %d cost = %v, want 1", i, u.Cost)
		}
	}
	if units[0].Payload != Heading || units[1].Payload != Paragraph {
		t.Errorf("payload builder saw kinds %v/%v, want Heading/Paragraph", units[0].Payload, units[1].Payload)
	}
	if len(units[1].Anchors) != 1 || units[1].Anchors[0] != "glossary/dek" {
		t.Errorf("unit anchors = %v, want the block's declared anchor", units[1].Anchors)
	}
}

// TestTokenizeOnePerBlockEmptyDocument proves an empty document mints nothing at
// all — in particular it must not index into a zero-length slice to set the
// content assertion.
func TestTokenizeOnePerBlockEmptyDocument(t *testing.T) {
	units := TokenizeOnePerBlock(publish.Document{Group: "node:a.md"},
		func(DecodedBlock) publish.BackendBlock { return nil })
	if len(units) != 0 {
		t.Errorf("units = %d, want none for a blockless document", len(units))
	}
}
