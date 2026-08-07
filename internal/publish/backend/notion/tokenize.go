package notion

import (
	"fmt"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/graph"
)

// Tokenize breaks a SetContent Document into Notion atomic block units. It
// consumes the shared parser's already-parsed neutral tree (graph.BlockContent) —
// it never re-parses Markdown — and reshapes each block into one or more Notion
// child blocks:
//
//   - one unit per block, Cost 1 (each unit is exactly one Notion child block, so
//     the Bin's ≤100-children ceiling is a plain sum of unit Costs);
//   - the group is the document's target node, so every unit shares an affinity
//     key and cannot bundle across pages;
//   - a block whose literal text exceeds the per-block char cap is SPLIT into
//     several units during tokenization (the second of Notion's two coupled
//     limits), each unit's text within the cap — and because splitting adds
//     blocks, it also pushes against the ≤100 ceiling;
//   - each block's late-bound Refs survive as inline placeholders on the unit that
//     carries them, and its declared Anchors are reported on the first unit (the
//     block that will host the anchor's Notion id) so the anchor map can be built.
func (b *Backend) Tokenize(doc publish.Document) []publish.AtomicUnit {
	var units []publish.AtomicUnit
	for _, blk := range doc.Blocks {
		// A table is a single, unsplittable unit: its rows are the block's children,
		// so it never chunks against the per-block char cap the way a flat run does.
		if bc, ok := blk.Content.(graph.BlockContent); ok && bc.Kind == graph.Table {
			cb, refs := tableChildBlock(bc)
			u := publish.AtomicUnit{Payload: cb, Cost: 1, Group: doc.Group, Refs: refs}
			if len(blk.Anchors) > 0 {
				u.Anchors = blk.Anchors
			}
			units = append(units, u)
			continue
		}
		kind, level, language, runs, hadInlineRefs := graph.RunsOf(blk.Content)
		chunks := b.splitRuns(runs)

		for i, chunk := range chunks {
			u := publish.AtomicUnit{
				Payload: childBlock{kind: int(kind), level: level, language: language, runs: chunk},
				Cost:    1,
				Group:   doc.Group,
				Refs:    publish.RefsOf(chunk),
			}
			// If the neutral content exposed no inline refs (a fallback content
			// shape), fall back to the block's aggregate Refs, hosted by the first
			// unit so late-bound references are never dropped.
			if !hadInlineRefs && i == 0 && len(blk.Refs) > 0 {
				u.Refs = append(u.Refs, blk.Refs...)
			}
			// The declared anchors live on the first unit — the block whose Notion id
			// becomes the anchor's target.
			if i == 0 && len(blk.Anchors) > 0 {
				u.Anchors = blk.Anchors
			}
			units = append(units, u)
		}
	}
	// One Document is one node's complete expected content, so its first unit is
	// where that assertion starts; every later unit is a continuation of the same
	// assertion. Marking it here — the one place a node's content is turned into
	// units — is what lets the Bin tell the two apart downstream (#130).
	if len(units) > 0 {
		if cb, ok := units[0].Payload.(childBlock); ok {
			cb.assertsContent = true
			units[0].Payload = cb
		}
	}
	return units
}

// TokenizeOp mints the single Notion AtomicUnit for a non-content op, carrying the
// backend payload and Cost the Bin needs to fuse: a create or properties unit
// costs zero children (it is the page itself, folded into POST /pages), a delete
// unit stands alone. The optimizer stamps the unit's Group and Refs.
func (b *Backend) TokenizeOp(op publish.NonContentOp) publish.AtomicUnit {
	switch op.Kind {
	case publish.CreateOp:
		return publish.AtomicUnit{Payload: createBlock{node: op.Node, parent: op.Parent}, Cost: 0}
	case publish.PropertiesOp:
		return publish.AtomicUnit{Payload: propsBlock{node: op.Node, parent: op.Parent, props: op.Props}, Cost: 0}
	case publish.DeleteOp:
		return publish.AtomicUnit{Payload: deleteBlock{node: op.Node}, Cost: 0}
	default:
		panic(fmt.Sprintf("notion: TokenizeOp got unknown NonContentOpKind %d", op.Kind))
	}
}

// tableChildBlock wraps the shared table flatten (graph.TableRunsOf) into Notion's
// single table childBlock — rows of cells of inline runs — and returns the symbolic
// ids of every Ref cited in any cell, in row-major order, so the AtomicUnit exposes
// them for the transport to gate and resolve. A table is never split against the
// char cap, so it maps one-to-one to one unit; a cell whose text exceeds Notion's
// per-span cap is left intact (these mirror tables are far under it).
func tableChildBlock(bc graph.BlockContent) (childBlock, []publish.SymbolicID) {
	rowRuns, refs := graph.TableRunsOf(bc)
	rows := make([]tableRow, 0, len(rowRuns))
	for _, cells := range rowRuns {
		rows = append(rows, tableRow{cells: cells})
	}
	return childBlock{kind: int(graph.Table), rows: rows, hasColumnHeader: bc.HasColumnHeader}, refs
}

// splitRuns packs a block's inline runs into chunks whose literal text stays
// within the per-block char cap. A text run longer than the cap is split across
// chunk boundaries; a Ref run costs no characters and rides in the current chunk.
// It always yields at least one chunk, so an empty block still emits one unit.
func (b *Backend) splitRuns(runs []publish.Run) [][]publish.Run {
	limit := b.maxChars
	var chunks [][]publish.Run
	var cur []publish.Run
	curChars := 0

	for _, r := range runs {
		if r.Ref != "" {
			cur = append(cur, r)
			continue
		}
		if r.Link != "" {
			// A hyperlink run rides whole: splitting its text mid-run would fracture
			// the link across spans (each span carries its own link object). If it
			// would overflow the current chunk, start a fresh one first. A single
			// link whose text exceeds the cap is left intact — the banner's text is
			// far under it, and a fractured link is worse than one oversized span.
			rlen := len([]rune(r.Text))
			if curChars > 0 && curChars+rlen > limit {
				chunks = append(chunks, cur)
				cur, curChars = nil, 0
			}
			cur = append(cur, r)
			curChars += rlen
			continue
		}
		rs := []rune(r.Text)
		for len(rs) > 0 {
			if curChars >= limit {
				chunks = append(chunks, cur)
				cur, curChars = nil, 0
			}
			take := limit - curChars
			if take > len(rs) {
				take = len(rs)
			}
			cur = append(cur, publish.Run{Text: string(rs[:take])})
			curChars += take
			rs = rs[take:]
		}
	}
	chunks = append(chunks, cur)
	return chunks
}

// splitByChars slices s into consecutive chunks whose rune length stays within
// limit — the per-span char cap Notion enforces on every rich_text object. It cuts
// on rune boundaries (never mid-rune) and always yields at least one chunk, so an
// empty string still produces one (empty) span. This is the plain-string analogue
// of splitRuns, which applies the same cap to a block's inline runs; property
// values (write-back columns, page properties) route through it via richTextSpans.
func splitByChars(s string, limit int) []string {
	rs := []rune(s)
	if limit <= 0 || len(rs) <= limit {
		return []string{s}
	}
	var chunks []string
	for len(rs) > limit {
		chunks = append(chunks, string(rs[:limit]))
		rs = rs[limit:]
	}
	return append(chunks, string(rs))
}
