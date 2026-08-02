package graph

import (
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// lineMapper maps byte offsets in a document body to 1-based file line numbers,
// so a neutral block can be matched back to the glossary anchor declared on its
// line. It mirrors the parser's own mapper (the two see the identical AST), kept
// local so the parser stays free of publisher concerns.
type lineMapper struct {
	starts    []int // byte offset of the start of each body line
	fileStart int   // file line number of body line 1
}

func newLineMapper(src []byte, fileStart int) *lineMapper {
	starts := []int{0}
	for i, c := range src {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	return &lineMapper{starts: starts, fileStart: fileStart}
}

// lineOf returns the file line of node n, preferring its first text segment and
// falling back to its enclosing block's first line.
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
