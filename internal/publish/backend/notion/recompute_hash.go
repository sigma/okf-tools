package notion

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/graph"
)

// This file holds the two halves of the recompute hash-alignment contract (#110):
// a source-side content hasher and the live-scan serializer, both projecting to
// one canonical block representation so an unchanged node hash-skips under
// --recompute. hash.go states the contract ("a backend whose scanner rebuilds a
// matching hash swaps this out via WithHasher so both sides agree"); this is that
// backend's implementation of it.
//
// The canonical representation is the *realized* Notion block stream — what the
// Executor actually writes — not the idealized source markdown: heading levels are
// clamped (notionBlockType), a long block is chunked by splitRuns, a table expands
// to its table_row children, and the block-0 banner rides as a quote. Both sides
// derive their (type, text) lines from that same projection, so agreement holds by
// construction rather than by two hand-matched serializers drifting apart.

const (
	// canonRefPlaceholder stands in for an inline cross-reference on both sides. A
	// source Ref carries no authored text; the Notion page mention it becomes
	// carries the *target's title* as server-filled plain_text. Neither side can
	// cheaply reproduce the other's rendering, so both fold a ref to this fixed
	// marker — a ref retarget is thus invisible to drift detection (acceptable: the
	// repo is the source of truth, and a real edit around the ref re-heals it).
	canonRefPlaceholder = "￼"
	// canonCellSep joins a table row's cells, and canonLinkSep separates a run's
	// visible text from an external link URL, so a URL change re-publishes even
	// when the visible label is untouched — the block-0 banner's source deep-link
	// and, since #119, every authored external link. Both are control chars that
	// cannot occur in authored prose.
	canonCellSep = "\x1f"
	canonLinkSep = "\x02"
)

// Notion block and run type strings the recompute scan and serializer switch on,
// named so the three sites that reason about a table's nested rows (listLiveBlocks
// recursion, parseLiveBlock cell decode, canonBlocksOfPayload projection) share one
// source. A table's rows are its own child blocks (nTypeTableRow), reachable only by
// recursing; a page mention (nTypeMention) folds to the ref placeholder; a
// child_page (nTypeChildPage) is its own node, excluded from a page's own hash.
const (
	nTypeTable     = "table"
	nTypeTableRow  = "table_row"
	nTypeChildPage = "child_page"
	nTypeMention   = "mention"
)

// canonBlock is one line of the canonical content fingerprint: a Notion block
// type and its extracted text. child_page blocks never appear here (they are their
// own nodes); a table appears as a "table" line followed by one "table_row" line
// per row.
type canonBlock struct {
	typ  string
	text string
}

// hashCanonBlocks is the single serializer both sides run: SHA-256 over one
// "<type>:<text>\n" line per block, in order.
func hashCanonBlocks(blocks []canonBlock) publish.Hash {
	h := sha256.New()
	for _, blk := range blocks {
		fmt.Fprintf(h, "%s:%s\n", blk.typ, blk.text)
	}
	return publish.Hash(hex.EncodeToString(h.Sum(nil)))
}

// The Notion backend is the one Recomputer: it can reconstruct a content hash
// from its live scan, so it supplies the source-side hasher. This static assertion
// guarantees a method-signature drift becomes a compile error rather than silently
// dropping the pipeline into its no-hasher fail branch (which re-clobbers every
// node — the #110 regression the named role guards against).
var _ graph.Recomputer = (*Backend)(nil)

// RecomputeContentHasher returns the source-side content hasher the pipeline wires
// via graph.WithHasher so change detection compares like against like: it projects
// each doc through the Executor's own transform (block-0 banner included) and
// fingerprints the realized blocks — exactly what liveContentHash reconstructs from
// live Notion blocks. The banner is captured here, so this hasher owns banner
// handling and graph does not re-fold it into the hash.
func (b *Backend) RecomputeContentHasher(bn *graph.Banner) func(*bundle.Doc) publish.Hash {
	return func(d *bundle.Doc) publish.Hash { return b.sourceContentHash(d, bn) }
}

// encodeHashPair joins a node's content and property hashes into the single value
// the `hash` derived column (and a subtree entry) stores, so the two-hash split
// (#110 phase 2) needs no new Notion column. Both halves are hex SHA-256, so the "."
// separator cannot occur inside either half.
func encodeHashPair(content, prop publish.Hash) string {
	return string(content) + "." + string(prop)
}

// decodeHashPair splits a stored hash value into its content and property halves. A
// legacy single-hash value (no separator, written before phase 2) decodes as a
// content hash with an empty property hash, so the first post-deploy scan re-asserts
// once and then steady-states — the one-time re-hash retiring the markdown hash
// already forces regardless.
func decodeHashPair(s string) (content, prop publish.Hash) {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return publish.Hash(s[:i]), publish.Hash(s[i+1:])
	}
	return publish.Hash(s), ""
}

// sourceContentHash is the source half of the alignment: it projects a doc through
// the very transform the Executor uses (ProjectDocument → Tokenize, so heading
// clamp, splitRuns chunking, and block-0 banner all match the realized page) and
// fingerprints the resulting child blocks. Wired in via graph.WithHasher on the
// --recompute path so the gate compares like against like.
func (b *Backend) sourceContentHash(d *bundle.Doc, bn *graph.Banner) publish.Hash {
	units := b.Tokenize(graph.ProjectDocument(d, bn))
	var blocks []canonBlock
	for _, u := range units {
		blocks = append(blocks, canonBlocksOfPayload(u.Payload)...)
	}
	return hashCanonBlocks(blocks)
}

// canonBlocksOfPayload projects one tokenized child block to its canonical
// line(s): a table to a "table" line plus a "table_row" line per row, any other
// block to a single line keyed by its realized Notion type.
func canonBlocksOfPayload(p publish.BackendBlock) []canonBlock {
	cb, ok := p.(childBlock)
	if !ok {
		return nil
	}
	if cb.kind == int(graph.Table) {
		out := []canonBlock{{typ: nTypeTable}}
		for _, row := range cb.rows {
			out = append(out, canonBlock{typ: nTypeTableRow, text: canonRowText(row)})
		}
		return out
	}
	return []canonBlock{{typ: notionBlockType(cb.kind, cb.level), text: canonRunsText(cb.runs)}}
}

// canonRunsText extracts a source run slice's canonical text: literal text (plus
// its external link URL, so a banner deep-link change is caught), with each Ref
// folded to the fixed placeholder.
func canonRunsText(runs []publish.Run) string {
	var s strings.Builder
	for _, r := range runs {
		if r.Ref != "" {
			s.WriteString(canonRefPlaceholder)
			continue
		}
		s.WriteString(r.Text)
		if r.Link != "" {
			s.WriteString(canonLinkSep)
			s.WriteString(r.Link)
		}
	}
	return s.String()
}

// canonRowText joins a source table row's cells with the cell separator, each cell
// serialized exactly as a block's runs are.
func canonRowText(row tableRow) string {
	parts := make([]string, len(row.cells))
	for i, cell := range row.cells {
		parts[i] = canonRunsText(cell)
	}
	return strings.Join(parts, canonCellSep)
}
