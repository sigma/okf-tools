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
		kind, level, runs, hadInlineRefs := reshape(blk.Content)
		chunks := b.splitRuns(runs)

		for i, chunk := range chunks {
			u := publish.AtomicUnit{
				Payload: childBlock{kind: kind, level: level, runs: chunk},
				Cost:    1,
				Group:   doc.Group,
				Refs:    refsOf(chunk),
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
	return units
}

// TokenizeOp mints the single Notion AtomicUnit for a non-content op, carrying the
// backend payload and Cost the Bin needs to fuse: a create or properties unit
// costs zero children (it is the page itself, folded into POST /pages), a delete
// unit stands alone. The optimizer stamps the unit's Group and Refs.
func (b *Backend) TokenizeOp(op publish.NonContentOp) publish.AtomicUnit {
	switch op.Kind {
	case publish.CreateOp:
		return publish.AtomicUnit{Payload: createBlock{node: op.Node}, Cost: 0}
	case publish.PropertiesOp:
		return publish.AtomicUnit{Payload: propsBlock{node: op.Node, props: op.Props}, Cost: 0}
	case publish.DeleteOp:
		return publish.AtomicUnit{Payload: deleteBlock{node: op.Node}, Cost: 0}
	default:
		panic(fmt.Sprintf("notion: TokenizeOp got unknown NonContentOpKind %d", op.Kind))
	}
}

// reshape projects a neutral block's opaque Content into an ordered run of Notion
// inline spans, reporting the block kind, level, and whether the content exposed
// any first-class inline Refs (so Tokenize knows whether to fall back to the
// block's aggregate Refs). It understands the shared parser's graph.BlockContent;
// a bare string or nil is tolerated as a degenerate single-run block.
func reshape(content any) (kind, level int, runs []textRun, hadInlineRefs bool) {
	switch c := content.(type) {
	case graph.BlockContent:
		kind, level = int(c.Kind), c.Level
		for _, in := range c.Inlines {
			if in.Ref != nil {
				runs = append(runs, textRun{Ref: in.Ref.ID})
				hadInlineRefs = true
				continue
			}
			if in.Text != "" {
				runs = append(runs, textRun{Text: in.Text})
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

// splitRuns packs a block's inline runs into chunks whose literal text stays
// within the per-block char cap. A text run longer than the cap is split across
// chunk boundaries; a Ref run costs no characters and rides in the current chunk.
// It always yields at least one chunk, so an empty block still emits one unit.
func (b *Backend) splitRuns(runs []textRun) [][]textRun {
	limit := b.maxChars
	var chunks [][]textRun
	var cur []textRun
	curChars := 0

	for _, r := range runs {
		if r.Ref != "" {
			cur = append(cur, r)
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
			cur = append(cur, textRun{Text: string(rs[:take])})
			curChars += take
			rs = rs[take:]
		}
	}
	chunks = append(chunks, cur)
	return chunks
}

// refsOf collects the symbolic ids of the Ref runs in one chunk, in order.
func refsOf(chunk []textRun) []publish.SymbolicID {
	var refs []publish.SymbolicID
	for _, r := range chunk {
		if r.Ref != "" {
			refs = append(refs, r.Ref)
		}
	}
	return refs
}
