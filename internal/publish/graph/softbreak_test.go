package graph

import "testing"

// firstParagraph returns the inline text of the first paragraph block, which is
// what a soft-wrapped source line has to survive into.
func firstParagraph(t *testing.T, body string) BlockContent {
	t.Helper()
	for _, bc := range blocksFromBody(t, body) {
		if bc.Kind == Paragraph {
			return bc
		}
	}
	t.Fatalf("no paragraph block in %q", body)
	return BlockContent{}
}

// TestSoftWrappedParagraphKeepsItsSpaces is the #181 regression. goldmark splits
// a wrapped paragraph into one *ast.Text per source line and records the break on
// the node that PRECEDES it; the segment excludes the newline and the whitespace
// before it, so concatenating segments glued the last word of each line to the
// first of the next — in every backend, since this is the shared builder.
func TestSoftWrappedParagraphKeepsItsSpaces(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{
			name: "plain wrap",
			body: "one two\nthree four\n",
			want: "one two three four",
		},
		{
			// The break falls on a style boundary: the next line OPENS an emphasis
			// span, which is where the reported corruption ("NotionArboraOS") landed.
			name: "break before emphasis",
			body: "carrying the Notion\n*ArboraOS Alpha Blueprint* drafts\n",
			want: "carrying the Notion ArboraOS Alpha Blueprint drafts",
		},
		{
			// And the mirror case: the line ENDS with the emphasis, so the break is
			// recorded on a text node inside it.
			name: "break after emphasis",
			body: "every part is *draft*\nnothing here is accepted\n",
			want: "every part is draft nothing here is accepted",
		},
		{
			name: "break after code span",
			body: "every part is `status: draft`\nnothing here is accepted\n",
			want: "every part is status: draft nothing here is accepted",
		},
		{
			name: "three lines",
			body: "alpha\nbeta\ngamma\n",
			want: "alpha beta gamma",
		},
		{
			// A hard break is two trailing spaces, and CommonMark renders it as a
			// newline rather than a space.
			name: "hard break, two spaces",
			body: "one  \ntwo\n",
			want: "one\ntwo",
		},
		{
			name: "hard break, backslash",
			body: "one\\\ntwo\n",
			want: "one\ntwo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inlineText(firstParagraph(t, tc.body)); got != tc.want {
				t.Errorf("inline text = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSoftWrappedListItemKeepsItsSpaces: a wrapped list item runs through the
// same inline collection, and bundle prose wraps bullets as readily as
// paragraphs.
func TestSoftWrappedListItemKeepsItsSpaces(t *testing.T) {
	var item BlockContent
	for _, bc := range blocksFromBody(t, "- a bullet that runs\n  onto a second line\n") {
		if bc.Kind == ListItem {
			item = bc
		}
	}
	if got := inlineText(item); got != "a bullet that runs onto a second line" {
		t.Errorf("list item text = %q, want the two lines separated by a space", got)
	}
}

// TestBreakAtALinkBoundaryStaysOutsideTheLink pins appendBreak's non-folding
// branch: a break recorded where the line ends on a link must not be folded into
// the linked run, or the hyperlink would cover a trailing space (or, for a Ref,
// there would be no text to fold into at all).
func TestBreakAtALinkBoundaryStaysOutsideTheLink(t *testing.T) {
	ref := &Ref{ID: "node:beta.md"}
	cases := []struct {
		name string
		in   []Inline
		brk  string
		want []Inline
	}{
		{
			name: "after a hyperlinked run",
			in:   []Inline{{Text: "see ", URL: ""}, {Text: "Beta", URL: "https://example/beta"}},
			brk:  " ",
			want: []Inline{{Text: "see "}, {Text: "Beta", URL: "https://example/beta"}, {Text: " "}},
		},
		{
			name: "after a Ref",
			in:   []Inline{{Text: "see "}, {Ref: ref}},
			brk:  " ",
			want: []Inline{{Text: "see "}, {Ref: ref}, {Text: " "}},
		},
		{
			// The break of a line ending in a link is recorded on an EMPTY text node
			// after it. When the link produced no inline — unresolved, or demoted out
			// of the selection — the preceding text already ends with the space.
			name: "the link produced nothing",
			in:   []Inline{{Text: "see "}},
			brk:  " ",
			want: []Inline{{Text: "see "}},
		},
		{
			name: "a hard break is never swallowed",
			in:   []Inline{{Text: "see "}},
			brk:  "\n",
			want: []Inline{{Text: "see \n"}},
		},
		{
			name: "folded into plain text",
			in:   []Inline{{Text: "one"}},
			brk:  " ",
			want: []Inline{{Text: "one "}},
		},
		{
			name: "at the start of a run",
			in:   nil,
			brk:  " ",
			want: []Inline{{Text: " "}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := appendBreak(tc.in, tc.brk)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d inlines %+v, want %d %+v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i].Text != tc.want[i].Text || got[i].URL != tc.want[i].URL || got[i].Ref != tc.want[i].Ref {
					t.Errorf("inline %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
