package graph

import (
	"strings"

	"github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"

	"github.com/sigma/okf-tools/internal/areas"
	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/parser"
	"github.com/sigma/okf-tools/internal/publish"
	"go.abhg.dev/goldmark/wikilink"
)

// BlockKind classifies a block-level node of the neutral document tree.
type BlockKind int

const (
	// Paragraph is a plain text block (or a loose-list paragraph).
	Paragraph BlockKind = iota
	// Heading is a heading; Level carries its 1-6 depth.
	Heading
	// ListItem is a list item; Level carries its nesting depth (1 = top level).
	ListItem
	// CodeBlock is a fenced or indented code block (no inline refs).
	CodeBlock
	// Quote is a block quote, its nested content flattened into one inline run.
	Quote
	// Generic is any other block-level node, kept so its inline refs survive.
	Generic
	// Table is a GFM table: its cells live in Rows (header row first), each cell
	// carrying its own inline run so cell links stay first-class Refs.
	Table
)

func (k BlockKind) String() string {
	switch k {
	case Paragraph:
		return "paragraph"
	case Heading:
		return "heading"
	case ListItem:
		return "item"
	case CodeBlock:
		return "code"
	case Quote:
		return "quote"
	case Table:
		return "table"
	default:
		return "generic"
	}
}

// BlockContent is the neutral, backend-agnostic content of one block, carried
// opaquely in publish.Block.Content. It is the rich semantic tree #162 hands
// across the tokenizer seam: a block kind, a depth, and an ordered inline run in
// which cross-references survive as first-class Ref inlines. The tokenizer
// reshapes this into the backend's own blocks; the pipeline never reads it.
type BlockContent struct {
	Kind    BlockKind
	Level   int // heading level or list-item depth; 0 otherwise
	Inlines []Inline
	// Language is the fenced code block's info-string language (e.g. "yaml"),
	// empty for indented code or a bare fence. Only meaningful when Kind is
	// CodeBlock; a backend maps it to its own code-language vocabulary.
	Language string
	// Rows holds a table's rows, header row first, when Kind is Table; nil for
	// every other kind. A table's own inline content lives per-cell in Rows rather
	// than in Inlines, so cell links survive as first-class Refs.
	Rows []TableRow
	// HasColumnHeader reports whether the table's first row is a header row. A GFM
	// table always has one; only meaningful when Kind is Table.
	HasColumnHeader bool
}

// TableRow is one row of a Table block: its cells left-to-right.
type TableRow struct {
	Cells []TableCell
}

// TableCell is one cell of a table row: its own inline run, in which cross-
// references survive as first-class Ref inlines exactly as a paragraph's do.
type TableCell struct {
	Inlines []Inline
}

// Inline is one inline node of a block's content: a text run, a first-class Ref,
// or a text run carrying an external hyperlink (URL != ""). Ref and URL are
// mutually exclusive: a Ref is a late-bound internal reference (Text is then
// empty); a URL is an external link whose visible label is Text. Authored
// Markdown never mints URL inlines — they carry the synthetic disclaimer banner's
// source deep-link, the one external link the neutral model needs (ADR-0015).
type Inline struct {
	Text string
	Ref  *Ref
	// URL, when non-empty, makes this a plain text run hyperlinked to an external
	// address. Only meaningful when Ref is nil.
	URL string
}

// Ref is a first-class late-bound reference node. It holds only a symbolic id
// ("node:<path>" or "anchor:<name>"), never a backend id, and survives parsing
// and tokenization unchanged; the transport resolves it against the
// scan-seeded resolution table.
type Ref struct {
	ID publish.SymbolicID
}

// anchorName mints the neutral anchor name of a glossary slug, namespaced by the
// glossary role rather than the (retired) hardwired host filename — the
// "glossary/<slug>" of #162's worked example.
func anchorName(slug string) publish.AnchorName {
	return publish.AnchorName(areas.RoleGlossary + "/" + slug)
}

// buildDocument parses d's body once with the shared mdast parser (never regex)
// and projects it to the backend-neutral document SetContent carries: an ordered
// list of blocks whose inline runs preserve cross-references as first-class Ref
// nodes. It returns the document, the aggregate symbolic ids it embeds (for edge
// computation), and the anchors it declares (a glossary host's terms/headings).
//
// Concept cross-links become Ref{node:target}; links into a glossary host with a
// #fragment become Ref{anchor:glossary/slug}. Every other link (external, image,
// citation, dangling) stays plain text — no Ref, no edge. Classification reuses
// the bundle's own resolution (d.Resolved), matched to AST link nodes by their
// shared depth-first order, so generation never re-implements link parsing.
func buildDocument(d *bundle.Doc, banner *Banner) (doc *publish.Document, refs []publish.SymbolicID, anchors []publish.AnchorName) {
	group := publish.GroupKey(publish.NodeRef(d.Rel))
	doc = &publish.Document{Group: group}

	// The disclaimer banner rides as a stable block-0, ahead of all authored
	// content, on every mirrored page — including an index or otherwise empty page
	// (ADR-0015). It carries no refs or anchors, so edge assembly is untouched.
	if banner != nil {
		doc.Blocks = append(doc.Blocks, banner.block(d.Rel))
	}

	body := []byte(d.Body)
	if len(body) > 0 {
		root := parser.Markdown().Parser().Parse(text.NewReader(body))
		b := &docBuilder{doc: d, src: body, lm: parser.NewLineMapper(body, d.BodyStartLine)}
		b.walk(root, 0)
		doc.Blocks = append(doc.Blocks, b.blocks...)
		refs = b.refs
	}

	if d.Glossary {
		for _, a := range d.Anchors {
			anchors = append(anchors, anchorName(a.Slug))
		}
	}
	return doc, refs, anchors
}

// ProjectDocument builds the backend-neutral block stream for a source doc,
// exactly as SetContent carries it (block-0 banner included when bn != nil). A
// backend's recompute hasher reuses this to fingerprint the same realized content
// its live scanner reconstructs from Notion blocks — the source half of the
// WithHasher round-trip contract (see hash.go). Refs and anchors are irrelevant to
// a content fingerprint and dropped; only the ordered blocks matter.
func ProjectDocument(d *bundle.Doc, bn *Banner) publish.Document {
	doc, _, _ := buildDocument(d, bn)
	return *doc
}

// docBuilder accumulates the neutral blocks of one document during an AST walk.
// linkIdx is the running ordinal into doc.Resolved: the k-th link-like inline
// node encountered (in the same depth-first order the parser used) resolves to
// doc.Resolved[k]. That shared order is what lets generation reuse the bundle's
// classification without re-parsing links itself.
type docBuilder struct {
	doc     *bundle.Doc
	src     []byte
	lm      *parser.LineMapper
	blocks  []publish.Block
	refs    []publish.SymbolicID
	linkIdx int
}

// walk recurses over n's children, emitting one neutral block per block-level
// node and descending into containers (lists, list items, block quotes) so every
// inline — and thus every link ordinal — is visited in parser order. depth is
// the list-item nesting level.
func (b *docBuilder) walk(n ast.Node, depth int) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch v := c.(type) {
		case *ast.Heading:
			b.emit(Heading, v.Level, c)
		case *ast.Paragraph, *ast.TextBlock:
			b.emit(Paragraph, 0, c)
		case *ast.List:
			b.walk(c, depth+1) // items live one level deeper
		case *ast.ListItem:
			// A list item's own text is its first paragraph/text block; nested
			// lists follow. Emit the item's inline run, then recurse for children.
			b.emit(ListItem, max(depth, 1), c)
		case *ast.Blockquote:
			b.emit(Quote, 0, c)
		case *east.Table:
			// A GFM table: emit one Table block whose cells carry their own inline
			// runs. emitTable visits cells in the parser's own depth-first order, so
			// the link ordinal stays in lockstep with d.Resolved just as emit does.
			b.emitTable(v)
		case *ast.FencedCodeBlock:
			// Code carries no inline links, so it contributes nothing to linkIdx.
			// Its body lives in Lines() (not child inlines) and its fence names a
			// language, both of which emitCode captures.
			b.emitCode(v, string(v.Language(b.src)))
		case *ast.CodeBlock:
			// Indented code: same body-in-Lines() shape, but no fence language.
			b.emitCode(v, "")
		case *ast.ThematicBreak:
			// nothing to carry
		default:
			// Unknown block: keep it (and count any links inside) so nothing is lost.
			b.emit(Generic, 0, c)
		}
	}
}

// emit collects n's inline content into one neutral block of the given kind and
// level, records the block's Refs and (for a glossary host) any anchor it hosts,
// and appends it. For a ListItem it collects only the item's own inline run, not
// nested lists, then recurses into the item so nested lists become their own
// deeper Item blocks.
func (b *docBuilder) emit(kind BlockKind, level int, n ast.Node) {
	inlines, blockRefs := b.inlinesOf(n)

	blk := publish.Block{
		Content: BlockContent{Kind: kind, Level: level, Inlines: inlines},
		Refs:    blockRefs,
	}
	if b.doc.Glossary {
		if slug, ok := b.anchorAt(n); ok {
			blk.Anchors = []publish.AnchorName{anchorName(slug)}
		}
	}
	b.blocks = append(b.blocks, blk)
	b.refs = append(b.refs, blockRefs...)

	// A list item may hold nested lists after its own text; recurse so they emit
	// as deeper Item blocks in document order.
	if _, ok := n.(*ast.ListItem); ok {
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if list, ok := c.(*ast.List); ok {
				b.walk(list, level+1)
			}
		}
	}
}

// emitTable projects a GFM table node into one neutral Table block: the header
// row (from the TableHeader child) followed by the body rows (each TableRow child),
// every cell carrying its own inline run. It reuses inlinesOf per cell, so a cell's
// links become first-class Refs and — crucially — the link ordinal advances in the
// parser's own depth-first order (header cells, then each row left-to-right), keeping
// linkIdx in lockstep with d.Resolved exactly as the flat blocks do. Rows are
// normalized to the header's column count, since Notion requires a rectangular
// table; a short row is padded with empty cells and any overflow cell is dropped
// (its already-consumed ordinal is preserved, only its rendered content and edge
// go). A table hosts no glossary anchor, so none of emit's anchor bookkeeping applies.
func (b *docBuilder) emitTable(n *east.Table) {
	var rows []TableRow
	var cellRefs [][]publish.SymbolicID
	appendRow := func(host ast.Node) {
		var row TableRow
		for c := host.FirstChild(); c != nil; c = c.NextSibling() {
			cell, ok := c.(*east.TableCell)
			if !ok {
				continue
			}
			inlines, refs := b.inlinesOf(cell)
			row.Cells = append(row.Cells, TableCell{Inlines: inlines})
			cellRefs = append(cellRefs, refs)
		}
		rows = append(rows, row)
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch c.(type) {
		case *east.TableHeader, *east.TableRow:
			appendRow(c)
		}
	}

	width := 0
	if len(rows) > 0 {
		width = len(rows[0].Cells)
	}
	// Flatten the refs of the cells that survive normalization, in row-major order,
	// so a dropped overflow cell contributes no edge (its ordinal was already
	// consumed above and must not be rewound).
	var blockRefs []publish.SymbolicID
	ci := 0
	for i := range rows {
		kept := len(rows[i].Cells)
		if kept > width {
			kept = width
		}
		for j := 0; j < len(rows[i].Cells); j++ {
			if j < kept {
				blockRefs = append(blockRefs, cellRefs[ci]...)
			}
			ci++
		}
		rows[i].Cells = normalizeCells(rows[i].Cells, width)
	}

	blk := publish.Block{
		Content: BlockContent{Kind: Table, Rows: rows, HasColumnHeader: len(rows) > 0},
		Refs:    blockRefs,
	}
	b.blocks = append(b.blocks, blk)
	b.refs = append(b.refs, blockRefs...)
}

// normalizeCells pads cells with empty trailing cells (or truncates overflow) so a
// row is exactly width cells wide — the rectangular shape Notion's table_row
// requires. width 0 (a header-less, empty table) leaves the row untouched.
func normalizeCells(cells []TableCell, width int) []TableCell {
	if width == 0 {
		return cells
	}
	if len(cells) > width {
		return cells[:width]
	}
	for len(cells) < width {
		cells = append(cells, TableCell{})
	}
	return cells
}

// emitCode appends one CodeBlock whose content is the block's literal body and,
// for a fenced block, its info-string language. A code block's text lives in the
// AST node's Lines() segments rather than child inline nodes, so inlinesOf (which
// walks children) would drop it; emitCode reads the segments directly. Code
// carries no refs or hostable anchors, so none of emit's bookkeeping applies.
func (b *docBuilder) emitCode(n ast.Node, language string) {
	var inlines []Inline
	if text := codeText(n, b.src); text != "" {
		inlines = []Inline{{Text: text}}
	}
	b.blocks = append(b.blocks, publish.Block{
		Content: BlockContent{Kind: CodeBlock, Inlines: inlines, Language: language},
	})
}

// codeText reconstructs a code block's literal body from its source line
// segments, dropping a single trailing newline (the fence/block boundary) while
// preserving interior blank lines.
func codeText(n ast.Node, src []byte) string {
	lines := n.Lines()
	var sb strings.Builder
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		sb.Write(seg.Value(src))
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// inlinesOf collects the inline run of block n: text spans verbatim and
// link-like nodes turned into Ref inlines (concept / glossary-anchor) or plain
// text (everything else). It does not descend into nested block-level nodes
// (nested lists), which emit as their own blocks. Returns the inline run and the
// symbolic ids of any Refs it produced.
func (b *docBuilder) inlinesOf(n ast.Node) (inlines []Inline, refs []publish.SymbolicID) {
	var visit func(node ast.Node)
	visit = func(node ast.Node) {
		for c := node.FirstChild(); c != nil; c = c.NextSibling() {
			switch v := c.(type) {
			case *ast.Text:
				if s := string(v.Segment.Value(b.src)); s != "" {
					inlines = append(inlines, Inline{Text: s})
				}
			case *ast.String:
				if s := string(v.Value); s != "" {
					inlines = append(inlines, Inline{Text: s})
				}
			case *ast.Link, *ast.Image, *ast.AutoLink, *wikilink.Node:
				// A link-like node advances the ordinal in lockstep with the parser.
				rl := b.nextResolved()
				if id, ok := refOf(rl); ok {
					inlines = append(inlines, Inline{Ref: &Ref{ID: id}})
					refs = append(refs, id)
				} else if rl != nil {
					// Keep the link's visible text as a plain run; no reference.
					if txt := rl.Text; txt != "" {
						inlines = append(inlines, Inline{Text: txt})
					}
				}
				// The link's text/ref is captured above, so we don't render its
				// children — but the parser's walk counts every nested link-like
				// node too (e.g. the image inside a linked image), so consume those
				// ordinals to keep linkIdx in lockstep with d.Resolved.
				b.consumeNested(c)
			case *ast.Emphasis, *ast.CodeSpan:
				visit(c) // formatting wrappers: keep their inner text/links inline
			default:
				visit(c)
			}
		}
	}
	// For a list item, restrict to its own text block(s), skipping nested lists.
	if _, ok := n.(*ast.ListItem); ok {
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if _, isList := c.(*ast.List); isList {
				continue
			}
			visit(c)
		}
		return inlines, refs
	}
	visit(n)
	return inlines, refs
}

// nextResolved returns the resolved link for the current link ordinal and
// advances it. It returns nil if the ordinal outruns doc.Resolved (defensive:
// the shared parser keeps them in lockstep).
func (b *docBuilder) nextResolved() *bundle.ResolvedLink {
	i := b.linkIdx
	b.linkIdx++
	if i < 0 || i >= len(b.doc.Resolved) {
		return nil
	}
	return &b.doc.Resolved[i]
}

// consumeNested advances the ordinal past every link-like node inside n, matching
// the parser's full depth-first walk (which records an entry for a nested image
// or link even though the visible content is the enclosing link's). It emits
// nothing; it only keeps linkIdx aligned with d.Resolved.
func (b *docBuilder) consumeNested(n ast.Node) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch c.(type) {
		case *ast.Link, *ast.Image, *ast.AutoLink, *wikilink.Node:
			b.linkIdx++
		}
		b.consumeNested(c)
	}
}

// refOf decides whether a resolved link becomes a first-class Ref, and which
// symbolic id it carries. Only in-bundle concept cross-links do: a link into a
// glossary host with a #fragment is an anchor ref; any other resolved concept
// link is a node ref (existence, fragment dropped — content-refs-node targets the
// create). External, image, citation, wiki, in-page, and dangling links are not
// refs.
func refOf(rl *bundle.ResolvedLink) (publish.SymbolicID, bool) {
	if rl == nil || rl.Class != bundle.ClassConcept || rl.TargetDoc == nil {
		return "", false
	}
	// A concept link into a glossary host carrying a #fragment is an anchor ref; any
	// other resolved concept link is a node ref (fragment dropped). The glossary-vs-
	// heading split is bundle.ResolvedLink.AnchorTarget's, shared with the lint rules.
	if _, kind := rl.AnchorTarget(nil); kind == bundle.GlossaryAnchor {
		return publish.AnchorRef(anchorName(rl.Fragment)), true
	}
	return publish.NodeRef(rl.TargetDoc.Rel), true
}

// anchorAt reports the glossary slug this block hosts, if any, by matching the
// block's source line against the host's declared anchors (terms and headings).
// The anchor set on the op is authoritative for edges; this per-block placement
// feeds the backend's anchor map.
func (b *docBuilder) anchorAt(n ast.Node) (string, bool) {
	line := b.lm.LineOf(n)
	for _, a := range b.doc.Anchors {
		if a.Line == line {
			return a.Slug, true
		}
	}
	return "", false
}
