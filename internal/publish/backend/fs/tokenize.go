package fs

import (
	"fmt"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/graph"
)

// Tokenize breaks a SetContent Document into filesystem section units — one unit
// per block, and (unlike Notion) NEVER split and NEVER fused: each block becomes
// exactly one AtomicUnit that Execute writes to its own file. It consumes the
// shared parser's already-parsed neutral tree (graph.BlockContent) and never
// re-parses Markdown:
//
//   - one unit per block, Cost 1 (each unit is exactly one on-disk write);
//   - the group is the document's target node — the export directory the units
//     land in, and the affinity key the pipeline partitions on;
//   - each block's late-bound Refs survive as inline placeholders on its unit, and
//     its declared Anchors ride along so the anchor map can be built.
//
// There is no per-block size cap here: a filesystem file is unbounded, so the
// second of Notion's two coupled limits simply does not exist for this backend —
// a concrete demonstration that the char/block caps were Notion's, not the seam's.
func (b *Backend) Tokenize(doc publish.Document) []publish.AtomicUnit {
	units := make([]publish.AtomicUnit, 0, len(doc.Blocks))
	for _, blk := range doc.Blocks {
		if bc, ok := blk.Content.(graph.BlockContent); ok && bc.Kind == graph.Table {
			rows, refs := tableRows(bc)
			u := publish.AtomicUnit{
				Payload: contentBlock{kind: int(graph.Table), rows: rows, hasColumnHeader: bc.HasColumnHeader, anchors: blk.Anchors},
				Cost:    1,
				Group:   doc.Group,
				Refs:    refs,
				Anchors: blk.Anchors,
			}
			if len(refs) == 0 && len(blk.Refs) > 0 {
				u.Refs = append(u.Refs, blk.Refs...)
			}
			units = append(units, u)
			continue
		}
		kind, level, runs, hadInlineRefs := reshape(blk.Content)
		u := publish.AtomicUnit{
			Payload: contentBlock{kind: kind, level: level, runs: runs, anchors: blk.Anchors},
			Cost:    1,
			Group:   doc.Group,
			Refs:    refsOf(runs),
			Anchors: blk.Anchors,
		}
		// If the neutral content exposed no inline refs (a degenerate content
		// shape), fall back to the block's aggregate Refs so late-bound references
		// still form their dependency edges even without a rendered placeholder.
		if !hadInlineRefs && len(blk.Refs) > 0 {
			u.Refs = append(u.Refs, blk.Refs...)
		}
		units = append(units, u)
	}
	return units
}

// TokenizeOp mints the single filesystem AtomicUnit for a non-content op. Every
// unit costs 1 — the honest "1 unit = 1 write" measure, the exact inverse of
// Notion's Cost-0 create/props (which cost zero blocks precisely so they can fuse
// into a POST). Here nothing fuses, so nothing is free. The optimizer stamps the
// unit's Group and Refs.
func (b *Backend) TokenizeOp(op publish.NonContentOp) publish.AtomicUnit {
	switch op.Kind {
	case publish.CreateOp:
		return publish.AtomicUnit{Payload: createBlock{node: op.Node}, Cost: 1}
	case publish.PropertiesOp:
		return publish.AtomicUnit{Payload: propsBlock{node: op.Node, props: op.Props}, Cost: 1}
	case publish.DeleteOp:
		return publish.AtomicUnit{Payload: deleteBlock{node: op.Node}, Cost: 1}
	default:
		panic(fmt.Sprintf("fs: TokenizeOp got unknown NonContentOpKind %d", op.Kind))
	}
}

// reshape projects a neutral block's opaque Content into an ordered run of inline
// spans, reporting the block kind, level, and whether the content exposed any
// first-class inline Refs (so Tokenize knows whether to fall back to the block's
// aggregate Refs). It understands the shared parser's graph.BlockContent; a bare
// string or nil is tolerated as a degenerate single-run block.
func reshape(content any) (kind, level int, runs []textRun, hadInlineRefs bool) {
	switch c := content.(type) {
	case graph.BlockContent:
		kind, level = int(c.Kind), c.Level
		runs = inlinesToRuns(c.Inlines)
		for _, r := range runs {
			if r.Ref != "" {
				hadInlineRefs = true
				break
			}
		}
	case string:
		if c != "" {
			runs = append(runs, textRun{Text: c})
		}
	case nil:
		// empty block — still emits one unit
	default:
		runs = append(runs, textRun{Text: fmt.Sprint(c)})
	}
	return kind, level, runs, hadInlineRefs
}

// inlinesToRuns maps a neutral inline run to filesystem textRuns: a Ref inline
// becomes a late-bound Ref placeholder, a URL inline a hyperlinked span, and a plain
// text inline a literal span. Empty spans are dropped. Shared by reshape (block-level
// runs) and tableRows (per-cell runs) so both project inlines identically.
func inlinesToRuns(inlines []graph.Inline) []textRun {
	var runs []textRun
	for _, in := range inlines {
		switch {
		case in.Ref != nil:
			runs = append(runs, textRun{Ref: in.Ref.ID})
		case in.URL != "":
			runs = append(runs, textRun{Text: in.Text, Link: in.URL})
		case in.Text != "":
			runs = append(runs, textRun{Text: in.Text})
		}
	}
	return runs
}

// tableRows reshapes a neutral Table block into its rows-of-cells-of-runs and the
// symbolic ids of every Ref cited in any cell, in row-major order, so the section's
// unit exposes them for the transport to gate and resolve.
func tableRows(bc graph.BlockContent) ([][][]textRun, []publish.SymbolicID) {
	var refs []publish.SymbolicID
	rows := make([][][]textRun, 0, len(bc.Rows))
	for _, r := range bc.Rows {
		cells := make([][]textRun, 0, len(r.Cells))
		for _, cell := range r.Cells {
			runs := inlinesToRuns(cell.Inlines)
			for _, run := range runs {
				if run.Ref != "" {
					refs = append(refs, run.Ref)
				}
			}
			cells = append(cells, runs)
		}
		rows = append(rows, cells)
	}
	return rows, refs
}

// refsOf collects the symbolic ids of the Ref runs in a block, in order.
func refsOf(runs []textRun) []publish.SymbolicID {
	var refs []publish.SymbolicID
	for _, r := range runs {
		if r.Ref != "" {
			refs = append(refs, r.Ref)
		}
	}
	return refs
}
