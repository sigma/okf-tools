package parser

import (
	"testing"

	"github.com/yuin/goldmark/text"
)

// A link label wrapped across a source line is one *ast.Text per line, with the
// break recorded on the first — and the segment excludes the newline and the
// whitespace before it, so the extracted text used to glue the two words (#181).
func TestWrappedLinkTextKeepsItsSpace(t *testing.T) {
	d := Parse("x.md", []byte("---\ntype: T\n---\n\nSee [the processing\nbackend](/backend.md).\n"))
	if len(d.Links) != 1 {
		t.Fatalf("links = %d, want 1", len(d.Links))
	}
	if got := d.Links[0].Text; got != "the processing backend" {
		t.Errorf("link text = %q, want %q", got, "the processing backend")
	}
}

// The same extraction feeds heading text, which is what anchors and headings
// matching read.
func TestWrappedTextExtractionKeepsItsSpaces(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"soft break", "one two\nthree four", "one two three four"},
		{"hard break", "one  \ntwo", "one\ntwo"},
		{"break before emphasis", "the Notion\n*Alpha* drafts", "the Notion Alpha drafts"},
		{"no break", "single line", "single line"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(tc.body + "\n")
			root := Markdown().Parser().Parse(text.NewReader(src))
			para := root.FirstChild()
			if got := collectText(para, src); got != tc.want {
				t.Errorf("collectText = %q, want %q", got, tc.want)
			}
		})
	}
}

// A CONTEXT-FORMAT term whose definition wraps must still be recognised: the
// colon check reads the inline run after the bold lead, which is where the break
// lands when the term sits at the end of a line.
func TestWrappedTermDefinitionIsStillWellFormed(t *testing.T) {
	d := Parse("x.md", []byte("---\ntype: T\n---\n\n- **Widget**: a small thing that\n  runs onto a second line.\n"))
	if len(d.Terms) != 1 {
		t.Fatalf("terms = %d, want 1 well-formed term (malformed: %+v)", len(d.Terms), d.MalformedTerms)
	}
	if d.Terms[0].Text != "Widget" {
		t.Errorf("term = %q, want %q", d.Terms[0].Text, "Widget")
	}
}
