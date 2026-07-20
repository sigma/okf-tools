// Package parser turns a markdown file into a Document: its YAML frontmatter
// (kept both as a decoded map and as an order-preserving yaml.Node), its body,
// and the markdown links and headings found in the body.
//
// The body is parsed with goldmark (a CommonMark-compliant parser) and its AST
// is walked — so fenced code blocks, inline code spans, reference links and
// nested brackets are handled by the parser, not by ad-hoc scanning. Obsidian
// [[wiki-links]] are recognised via the goldmark wikilink extension so OKF101
// can flag them. Frontmatter is split on its `---` delimiters (the same way
// goldmark-meta/Hugo do) rather than parsed, so the order-preserving yaml.Node
// survives for `okf fmt`.
//
// The package is deliberately config-agnostic — classifying links (concept
// cross-link vs citation vs external) lives in the bundle package.
package parser

import (
	"os"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"go.abhg.dev/goldmark/wikilink"
	"gopkg.in/yaml.v3"
)

// md is the shared CommonMark parser, with wikilink recognition enabled.
var md = goldmark.New(goldmark.WithExtensions(&wikilink.Extender{}))

// Document is the parsed representation of a single markdown file.
type Document struct {
	Path    string // path as supplied to Parse (usually absolute)
	Content string // raw file content, verbatim

	// Frontmatter block state. A conformant block opens with a line that is
	// exactly "---" at the very top of the file and closes with another such
	// line. FrontmatterRaw is the YAML text between the two delimiters.
	HasOpening     bool
	Terminated     bool
	FrontmatterRaw string
	Frontmatter    map[string]any // decoded YAML mapping (nil on error/absent)
	FrontmatterKey *yaml.Node     // the mapping node, for order-preserving re-emit
	ParseErr       error          // non-nil if the frontmatter is present but invalid YAML

	Body          string // everything after the closing delimiter (verbatim)
	BodyStartLine int    // 1-based line number of the first body line

	Links     []Link
	Headings  []Heading
	ListItems []ListItem
	Terms     []Term
}

// Link is a single markdown or wiki link found in the body.
type Link struct {
	Text        string // link text / label
	Target      string // raw target (URL or path)
	Line        int    // 1-based line number in the file
	Wikilink    bool   // [[...]] Obsidian syntax
	Image       bool   // ![...](...) image, not a navigational link
	InCitations bool   // set by MarkCitations: link sits under the citations heading
}

// Heading is a single heading found in the body.
type Heading struct {
	Level int
	Text  string // heading text, trimmed, without the leading #s
	Line  int    // 1-based line number in the file
}

// Term is a CONTEXT-FORMAT glossary entry: a paragraph or list item that leads
// with a bold span immediately followed by a colon (`**Side-door credential**:
// …`). Text is the raw bold term text, without the surrounding `**`. The
// glossary extension slugs this into an anchor; the parser only extracts it.
type Term struct {
	Text string // the bold lead text, e.g. "Side-door credential"
	Line int    // 1-based line number in the file
}

// ListItem is a single markdown list item found in the body. HasLink reports
// whether the item contains a navigational link (used by OKF003 to check that
// index entries are links).
type ListItem struct {
	Line    int
	HasLink bool
}

// HasFrontmatter reports whether the document carries a well-formed,
// parseable frontmatter block.
func (d *Document) HasFrontmatter() bool {
	return d.HasOpening && d.Terminated && d.ParseErr == nil
}

const bom = "\ufeff"

// ParseFile reads and parses the file at path.
func ParseFile(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(path, data), nil
}

// Parse parses data as a markdown document.
func Parse(path string, data []byte) *Document {
	content := strings.TrimPrefix(string(data), bom)
	d := &Document{Path: path, Content: content}

	lines := strings.Split(content, "\n")

	bodyLines := lines
	bodyStart := 1
	if len(lines) > 0 && delim(lines[0]) {
		d.HasOpening = true
		for i := 1; i < len(lines); i++ {
			if delim(lines[i]) {
				d.Terminated = true
				d.FrontmatterRaw = strings.Join(trimCR(lines[1:i]), "\n")
				bodyLines = lines[i+1:]
				bodyStart = i + 2
				break
			}
		}
		if !d.Terminated {
			// Unterminated block: no body, frontmatter is malformed.
			bodyLines = nil
		}
		d.decodeFrontmatter()
	}

	d.Body = strings.Join(bodyLines, "\n")
	d.BodyStartLine = bodyStart
	d.parseBody()
	return d
}

func (d *Document) decodeFrontmatter() {
	if strings.TrimSpace(d.FrontmatterRaw) == "" {
		d.Frontmatter = map[string]any{}
		return
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(d.FrontmatterRaw), &doc); err != nil {
		d.ParseErr = err
		return
	}
	m := map[string]any{}
	if err := doc.Decode(&m); err != nil {
		d.ParseErr = err
		return
	}
	d.Frontmatter = m
	if len(doc.Content) > 0 && doc.Content[0].Kind == yaml.MappingNode {
		d.FrontmatterKey = doc.Content[0]
	}
}

// parseBody walks the goldmark AST of the body, collecting headings and links
// with file-relative line numbers.
func (d *Document) parseBody() {
	src := []byte(d.Body)
	if len(src) == 0 {
		return
	}
	lm := newLineMapper(src, d.BodyStartLine)
	root := md.Parser().Parse(text.NewReader(src))

	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Heading:
			d.Headings = append(d.Headings, Heading{
				Level: v.Level,
				Text:  strings.TrimSpace(collectText(v, src)),
				Line:  lm.lineOf(v),
			})
		case *ast.ListItem:
			d.ListItems = append(d.ListItems, ListItem{
				Line:    lm.lineOf(v),
				HasLink: hasLinkDescendant(v),
			})
		case *ast.Paragraph:
			if term, ok := leadTerm(v, src); ok {
				d.Terms = append(d.Terms, Term{Text: term, Line: lm.lineOf(v)})
			}
		case *ast.TextBlock:
			// TextBlock is the inline container of a tight list item.
			if term, ok := leadTerm(v, src); ok {
				d.Terms = append(d.Terms, Term{Text: term, Line: lm.lineOf(v)})
			}
		case *ast.Link:
			d.Links = append(d.Links, Link{
				Text:   collectText(v, src),
				Target: string(v.Destination),
				Line:   lm.lineOf(v),
			})
		case *ast.Image:
			d.Links = append(d.Links, Link{
				Text:   collectText(v, src),
				Target: string(v.Destination),
				Line:   lm.lineOf(v),
				Image:  true,
			})
		case *ast.AutoLink:
			d.Links = append(d.Links, Link{
				Text:   string(v.URL(src)),
				Target: string(v.URL(src)),
				Line:   lm.lineOf(v),
			})
		case *wikilink.Node:
			target := string(v.Target)
			if len(v.Fragment) > 0 {
				target += "#" + string(v.Fragment)
			}
			text := collectText(v, src)
			if text == "" {
				text = target
			}
			d.Links = append(d.Links, Link{
				Text:     text,
				Target:   target,
				Line:     lm.lineOf(v),
				Wikilink: true,
				Image:    v.Embed,
			})
		}
		return ast.WalkContinue, nil
	})
}

// MarkCitations sets InCitations on every link that sits under a heading for
// which pred returns true, i.e. from that heading down to the next heading of
// equal-or-shallower level (or end of file).
func (d *Document) MarkCitations(pred func(Heading) bool) {
	for _, h := range d.Headings {
		if !pred(h) {
			continue
		}
		end := int(^uint(0) >> 1) // max int
		for _, nh := range d.Headings {
			if nh.Line > h.Line && nh.Level <= h.Level && nh.Line < end {
				end = nh.Line
			}
		}
		for i := range d.Links {
			if d.Links[i].Line > h.Line && d.Links[i].Line < end {
				d.Links[i].InCitations = true
			}
		}
	}
}

// leadTerm reports whether block n is a CONTEXT-FORMAT term entry: its first
// inline child is a bold span (`**…**`, i.e. an Emphasis of level 2) whose text
// is immediately followed by a colon. It returns the bold term text. A bold span
// mid-sentence, an italic `_Avoid_` lead (Emphasis level 1), or a missing colon
// all fail — so only well-formed `**Term**: …` entries are recognised.
func leadTerm(n ast.Node, src []byte) (string, bool) {
	lead := n.FirstChild()
	em, ok := lead.(*ast.Emphasis)
	if !ok || em.Level != 2 {
		return "", false
	}
	term := strings.TrimSpace(collectText(em, src))
	if term == "" {
		return "", false
	}
	// The remainder of the inline run must lead with the colon.
	var rest strings.Builder
	for c := em.NextSibling(); c != nil; c = c.NextSibling() {
		switch t := c.(type) {
		case *ast.Text:
			rest.Write(t.Segment.Value(src))
		case *ast.String:
			rest.Write(t.Value)
		default:
			rest.WriteString(collectText(c, src))
		}
	}
	if !strings.HasPrefix(rest.String(), ":") {
		return "", false
	}
	return term, true
}

// hasLinkDescendant reports whether n has a navigational link (markdown link,
// autolink, or wikilink) anywhere in its subtree.
func hasLinkDescendant(n ast.Node) bool {
	found := false
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch c.(type) {
		case *ast.Link, *ast.AutoLink, *wikilink.Node:
			found = true
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	return found
}

// collectText concatenates the source text of a node's inline descendants,
// yielding the rendered link/heading text without markup delimiters.
func collectText(n ast.Node, src []byte) string {
	var sb strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch t := c.(type) {
		case *ast.Text:
			sb.Write(t.Segment.Value(src))
		case *ast.String:
			sb.Write(t.Value)
		default:
			sb.WriteString(collectText(c, src))
		}
	}
	return sb.String()
}

// lineMapper maps byte offsets in the body source to 1-based file line numbers.
type lineMapper struct {
	starts    []int // byte offset of the start of each body line
	fileStart int   // file line number of body line 1
}

func newLineMapper(src []byte, fileStart int) *lineMapper {
	starts := []int{0}
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return &lineMapper{starts: starts, fileStart: fileStart}
}

// lineOf returns the file line number of node n, preferring the node's own
// first text segment and falling back to its enclosing block's first line.
func (m *lineMapper) lineOf(n ast.Node) int {
	if seg, ok := firstSegment(n); ok {
		return m.at(seg.Start)
	}
	if off, ok := blockFirstOffset(n); ok {
		return m.at(off)
	}
	return m.fileStart
}

func (m *lineMapper) at(offset int) int {
	lo, hi := 0, len(m.starts)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if m.starts[mid] <= offset {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return m.fileStart + lo
}

func firstSegment(n ast.Node) (text.Segment, bool) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if t, ok := c.(*ast.Text); ok {
			return t.Segment, true
		}
		if seg, ok := firstSegment(c); ok {
			return seg, true
		}
	}
	return text.Segment{}, false
}

func blockFirstOffset(n ast.Node) (int, bool) {
	for p := n; p != nil; p = p.Parent() {
		if p.Type() == ast.TypeBlock {
			if lines := p.Lines(); lines != nil && lines.Len() > 0 {
				return lines.At(0).Start, true
			}
		}
	}
	return 0, false
}

func delim(line string) bool { return strings.TrimRight(line, "\r") == "---" }

func trimCR(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimRight(l, "\r")
	}
	return out
}
