package gdocs

import (
	"fmt"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/graph"
)

// Tokenize breaks a document into one unit per block. Each unit is one
// batchUpdate request (insertText, plus its styling request), so Cost is the
// request count — the honest currency for a backend whose transaction is an
// ordered request array.
func (b *Backend) Tokenize(doc publish.Document) []publish.AtomicUnit {
	units := make([]publish.AtomicUnit, 0, len(doc.Blocks))
	for _, blk := range doc.Blocks {
		kind, level, _, runs, hadInlineRefs := graph.RunsOf(blk.Content)
		u := publish.AtomicUnit{
			Payload: contentBlock{kind: int(kind), level: level, runs: runs, anchors: blk.Anchors},
			// A heading costs 2 requests (insertText + updateParagraphStyle); a
			// paragraph costs 1. Cost is per-backend, so this arithmetic stays here.
			Cost:    costOf(kind),
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

func costOf(kind graph.BlockKind) int {
	switch kind {
	case graph.Heading, graph.CodeBlock, graph.Quote:
		return 2 // insert + style
	case graph.Table:
		return 3 // insertTable + cell inserts + style
	default:
		return 1
	}
}

// TokenizeOp mints the single unit for a non-content op.
func (b *Backend) TokenizeOp(op publish.NonContentOp) publish.AtomicUnit {
	switch op.Kind {
	case publish.CreateOp:
		return publish.AtomicUnit{Payload: createTab{node: op.Node}, Cost: 1}
	case publish.PropertiesOp:
		return publish.AtomicUnit{Payload: setProps{node: op.Node, props: op.Props}, Cost: 1}
	case publish.DeleteOp:
		return publish.AtomicUnit{Payload: deleteTab{node: op.Node}, Cost: 1}
	default:
		panic(fmt.Sprintf("gdocs: unknown NonContentOpKind %d", op.Kind))
	}
}
