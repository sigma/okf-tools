package graph

import "github.com/sigma/okf-tools/internal/publish"

// This file is the shared block WALK, the companion to runs.go's shared flatten.
// runs.go answered "how does one block's inline content become runs"; this answers
// "how does a Document's block become a tokenizer's unit" — the decode, the table
// carve-out, and the aggregate-Refs fallback that every backend needs before it can
// build its own payload.
//
// It exists because those steps were written three times, once per backend, and had
// begun to drift: two of the three tokenizers were identical line for line save for
// the payload struct they returned.

// DecodedBlock is one block of a neutral Document, decoded once into the shape a
// tokenizer actually builds a payload from. It resolves the two things every
// backend had to work out for itself: whether the block is a table (whose inline
// content lives per-cell, so it flattens differently and never splits), and which
// late-bound Refs the resulting unit must carry.
type DecodedBlock struct {
	// Kind, Level and Language describe the block. Kind is Table exactly when
	// IsTable is set.
	Kind     BlockKind
	Level    int
	Language string
	// IsTable distinguishes the two content shapes below: a table populates Rows
	// and leaves Runs empty, every other kind the reverse.
	IsTable bool
	// Runs is the block's flattened inline content; empty for a table.
	Runs []publish.Run
	// Rows carries a table's cells, header row first; nil for every other kind.
	Rows            [][][]publish.Run
	HasColumnHeader bool
	// Anchors are the anchors the block declares, to be reported on the unit that
	// will host them.
	Anchors []publish.AnchorName
	// Refs are the unit's late-bound refs with the aggregate fallback ALREADY
	// applied — what a tokenizer emitting one unit per block wants, and the reason
	// the fallback rule now has one home instead of three.
	Refs []publish.SymbolicID
	// HadInlineRefs and BlockRefs expose the fallback's inputs, for a tokenizer that
	// SPLITS a block across several units and so must place the fallback on the
	// first of them rather than on all of them. Notion is the only such backend
	// today; without these it would have to redo the decode to get at them.
	HadInlineRefs bool
	BlockRefs     []publish.SymbolicID
}

// DecodeBlock decodes one neutral block. The aggregate-Refs fallback it applies is
// the rule that a block whose content exposed no first-class inline Refs still
// contributes its declared Refs, so a late-bound reference in a degenerate content
// shape is never silently dropped and its dependency edge still forms.
func DecodeBlock(blk publish.Block) DecodedBlock {
	if bc, ok := blk.Content.(BlockContent); ok && bc.Kind == Table {
		rows, refs := TableRunsOf(bc)
		d := DecodedBlock{
			Kind:            Table,
			IsTable:         true,
			Rows:            rows,
			HasColumnHeader: bc.HasColumnHeader,
			Anchors:         blk.Anchors,
			Refs:            refs,
			HadInlineRefs:   len(refs) > 0,
			BlockRefs:       blk.Refs,
		}
		if len(refs) == 0 && len(blk.Refs) > 0 {
			d.Refs = append(d.Refs, blk.Refs...)
		}
		return d
	}

	kind, level, language, runs, hadInlineRefs := RunsOf(blk.Content)
	d := DecodedBlock{
		Kind:          kind,
		Level:         level,
		Language:      language,
		Runs:          runs,
		Anchors:       blk.Anchors,
		Refs:          publish.RefsOf(runs),
		HadInlineRefs: hadInlineRefs,
		BlockRefs:     blk.Refs,
	}
	if !hadInlineRefs && len(blk.Refs) > 0 {
		d.Refs = append(d.Refs, blk.Refs...)
	}
	return d
}

// TokenizeOnePerBlock is the whole tokenizer for a backend that neither splits nor
// fuses: every block becomes exactly one AtomicUnit costing 1, carrying the block's
// Refs and Anchors, and the Document's first unit asserts the node's content.
// payload supplies the only genuinely per-backend part — the opaque block the
// backend's own Executor will write.
//
// A backend with a per-unit ceiling (Notion's char cap) cannot use this, because
// splitting changes which unit carries what; such a backend calls DecodeBlock
// directly and does its own packing. That is the real difference between the two
// shapes, and it is now the only difference expressed in a tokenizer.
func TokenizeOnePerBlock(doc publish.Document, payload func(DecodedBlock) publish.BackendBlock) []publish.AtomicUnit {
	units := make([]publish.AtomicUnit, 0, len(doc.Blocks))
	for _, blk := range doc.Blocks {
		d := DecodeBlock(blk)
		units = append(units, publish.AtomicUnit{
			Payload: payload(d),
			Cost:    1,
			Group:   doc.Group,
			Refs:    d.Refs,
			Anchors: d.Anchors,
		})
	}
	if len(units) > 0 {
		units[0].AssertsContent = true
	}
	return units
}
