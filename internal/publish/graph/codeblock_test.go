package graph

import (
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/parser"
	"github.com/yuin/goldmark/text"
)

// blocksFromBody parses a Markdown body through the shared parser and returns the
// neutral BlockContent of each emitted block, for asserting the graph model
// captures a block's shape.
func blocksFromBody(t *testing.T, body string) []BlockContent {
	t.Helper()
	src := []byte(body)
	root := parser.Markdown().Parser().Parse(text.NewReader(src))
	b := &docBuilder{doc: &bundle.Doc{}, src: src, lm: newLineMapper(src, 1)}
	b.walk(root, 0)
	out := make([]BlockContent, 0, len(b.blocks))
	for _, blk := range b.blocks {
		out = append(out, blk.Content.(BlockContent))
	}
	return out
}

func onlyCodeBlock(t *testing.T, blocks []BlockContent) BlockContent {
	t.Helper()
	for _, bc := range blocks {
		if bc.Kind == CodeBlock {
			return bc
		}
	}
	t.Fatalf("no code block among %d blocks", len(blocks))
	return BlockContent{}
}

func inlineText(bc BlockContent) string {
	var sb strings.Builder
	for _, in := range bc.Inlines {
		sb.WriteString(in.Text)
	}
	return sb.String()
}

// A fenced code block must carry its info-string language and its literal body,
// both of which the neutral model dropped before #80.
func TestFencedCodeCarriesLanguageAndText(t *testing.T) {
	cb := onlyCodeBlock(t, blocksFromBody(t, "```yaml\nfoo: bar\nbaz: 1\n```\n"))
	if cb.Language != "yaml" {
		t.Errorf("Language = %q, want %q", cb.Language, "yaml")
	}
	if got := inlineText(cb); got != "foo: bar\nbaz: 1" {
		t.Errorf("code text = %q, want the fenced body", got)
	}
}

// A fence with no info string leaves Language empty (the executor defaults it).
func TestFencedCodeNoLanguage(t *testing.T) {
	cb := onlyCodeBlock(t, blocksFromBody(t, "```\nbare\n```\n"))
	if cb.Language != "" {
		t.Errorf("Language = %q, want empty", cb.Language)
	}
	if got := inlineText(cb); got != "bare" {
		t.Errorf("code text = %q, want %q", got, "bare")
	}
}

// An indented code block has no language but must still keep its text.
func TestIndentedCodeKeepsText(t *testing.T) {
	cb := onlyCodeBlock(t, blocksFromBody(t, "para\n\n    plain code\n    line two\n"))
	if cb.Language != "" {
		t.Errorf("indented code Language = %q, want empty", cb.Language)
	}
	if got := inlineText(cb); !strings.Contains(got, "plain code") || !strings.Contains(got, "line two") {
		t.Errorf("indented code text = %q, want it to keep both lines", got)
	}
}
