package gdocs

import (
	"fmt"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/graph"
)

// maxRequestsPerBatch bounds one batchUpdate. The API documents no request-count
// cap, so this is a self-imposed ceiling that keeps a single failed batch small
// enough to reason about — the whole batch is atomic, so a huge one is
// all-or-nothing over an entire document.
const maxRequestsPerBatch = 200

// Tokenize turns a document's blocks into units.
//
// This is the PLACEHOLDER tokenizer: every block becomes one plain paragraph, and
// the construct→request mapping from #150 (headings, lists, tables, images,
// inline styles, code shading) lands in #160. What is real here is the SHAPE —
// one unit per block, cost in requests, the group as the affinity key, and Refs
// and Anchors preserved so late binding and the anchor map survive.
func (b *Backend) Tokenize(doc publish.Document) []publish.AtomicUnit {
	units := make([]publish.AtomicUnit, 0, len(doc.Blocks))
	for _, blk := range doc.Blocks {
		// A table's inline content lives per-cell, so its refs come from the cells
		// rather than from a flat run list.
		if bc, ok := blk.Content.(graph.BlockContent); ok && bc.Kind == graph.Table {
			rows, refs := graph.TableRunsOf(bc)
			u := publish.AtomicUnit{
				Payload: contentBlock{kind: graph.Table, rows: rows, anchors: blk.Anchors},
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
		kind, level, _, runs, hadInlineRefs := graph.RunsOf(blk.Content)
		u := publish.AtomicUnit{
			Payload: contentBlock{kind: kind, level: level, runs: runs, anchors: blk.Anchors},
			Cost:    1,
			Group:   doc.Group,
			Refs:    publish.RefsOf(runs),
			Anchors: blk.Anchors,
		}
		if !hadInlineRefs && len(blk.Refs) > 0 {
			u.Refs = append(u.Refs, blk.Refs...)
		}
		units = append(units, u)
	}
	return units
}

// TokenizeOp mints the single unit for a non-content op. The optimizer stamps the
// unit's Group and Refs.
func (b *Backend) TokenizeOp(op publish.NonContentOp) publish.AtomicUnit {
	switch op.Kind {
	case publish.CreateOp:
		return publish.AtomicUnit{Payload: createTab{node: op.Node}, Cost: 1}
	case publish.PropertiesOp:
		// Properties have no home on a tab, so they are rendered as content — the
		// carve-out from #152. The unit still exists so the op flows through one path.
		return publish.AtomicUnit{Payload: setProps{node: op.Node, props: op.Props}, Cost: 1}
	case publish.DeleteOp:
		return publish.AtomicUnit{Payload: deleteTab{node: op.Node}, Cost: 1}
	default:
		panic(fmt.Sprintf("gdocs: unknown NonContentOpKind %d", op.Kind))
	}
}

// --- payloads (opaque to the pipeline) --------------------------------------

type createTab struct{ node publish.SymbolicID }

type deleteTab struct{ node publish.SymbolicID }

type setProps struct {
	node  publish.SymbolicID
	props map[string]any
}

type contentBlock struct {
	kind    graph.BlockKind
	level   int
	runs    []publish.Run
	anchors []publish.AnchorName
	// rows carries a table's cells, header row first; set only when kind is Table,
	// in which case runs is empty.
	rows [][][]publish.Run
}
