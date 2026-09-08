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
	return graph.TokenizeOnePerBlock(doc, func(d graph.DecodedBlock) publish.BackendBlock {
		if d.IsTable {
			return contentBlock{kind: int(graph.Table), rows: d.Rows, hasColumnHeader: d.HasColumnHeader, anchors: d.Anchors}
		}
		return contentBlock{kind: int(d.Kind), level: d.Level, runs: d.Runs, anchors: d.Anchors}
	})
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
