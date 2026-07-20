package parser

import "testing"

func TestFrontmatterSplit(t *testing.T) {
	src := "---\ntype: Thing\ntitle: Foo\n---\n\n# Body\n"
	d := Parse("x.md", []byte(src))
	if !d.HasFrontmatter() {
		t.Fatalf("expected frontmatter, got HasOpening=%v Terminated=%v err=%v", d.HasOpening, d.Terminated, d.ParseErr)
	}
	if d.Frontmatter["type"] != "Thing" {
		t.Errorf("type = %v, want Thing", d.Frontmatter["type"])
	}
	if d.BodyStartLine != 5 {
		t.Errorf("BodyStartLine = %d, want 5", d.BodyStartLine)
	}
}

func TestUnterminatedFrontmatter(t *testing.T) {
	d := Parse("x.md", []byte("---\ntype: Thing\n"))
	if d.HasFrontmatter() {
		t.Error("unterminated frontmatter should not be valid")
	}
	if !d.HasOpening || d.Terminated {
		t.Errorf("HasOpening=%v Terminated=%v, want true/false", d.HasOpening, d.Terminated)
	}
}

func TestInvalidYAML(t *testing.T) {
	d := Parse("x.md", []byte("---\ntype: : :\n bad\n---\n"))
	if d.ParseErr == nil {
		t.Error("expected a YAML parse error")
	}
}

func TestLinksAndLines(t *testing.T) {
	src := "---\ntype: T\n---\n" + // lines 1-3
		"# Title\n" + // line 4
		"\n" + // line 5
		"See [neo4j](/neo4j.md) and [graph](graphrag.md).\n" + // line 6
		"\n" + // line 7
		"```\n[code](should-not-count.md)\n```\n" + // lines 8-10 (fenced)
		"An `inline [code](nope.md)` span.\n" // line 11
	d := Parse("x.md", []byte(src))
	if len(d.Links) != 2 {
		for _, l := range d.Links {
			t.Logf("link: %+v", l)
		}
		t.Fatalf("got %d links, want 2 (code-block and code-span links excluded)", len(d.Links))
	}
	if d.Links[0].Target != "/neo4j.md" || d.Links[0].Line != 6 {
		t.Errorf("link0 = %+v, want target /neo4j.md line 6", d.Links[0])
	}
	if d.Links[1].Target != "graphrag.md" || d.Links[1].Line != 6 {
		t.Errorf("link1 = %+v, want target graphrag.md line 6", d.Links[1])
	}
}

func TestWikilink(t *testing.T) {
	d := Parse("x.md", []byte("body [[Foo Bar|baz]] end\n"))
	if len(d.Links) != 1 || !d.Links[0].Wikilink {
		t.Fatalf("expected 1 wikilink, got %+v", d.Links)
	}
	if d.Links[0].Target != "Foo Bar" || d.Links[0].Text != "baz" {
		t.Errorf("wikilink = %+v, want target 'Foo Bar' text 'baz'", d.Links[0])
	}
}

func TestCitations(t *testing.T) {
	src := "# Body\n" +
		"Link [a](a.md).\n" +
		"\n" +
		"# Citations\n" +
		"[1] [Source](https://example.com)\n"
	d := Parse("x.md", []byte(src))
	d.MarkCitations(func(h Heading) bool { return h.Text == "Citations" })
	var cite, body *Link
	for i := range d.Links {
		if d.Links[i].InCitations {
			cite = &d.Links[i]
		} else {
			body = &d.Links[i]
		}
	}
	if body == nil || body.Target != "a.md" {
		t.Errorf("body link = %+v, want a.md not in citations", body)
	}
	if cite == nil || cite.Target != "https://example.com" {
		t.Errorf("citation link = %+v, want example.com in citations", cite)
	}
}

func TestTermExtraction(t *testing.T) {
	src := "---\ntype: T\n---\n" + // lines 1-3
		"# Terms\n" + // line 4
		"\n" + // line 5
		"**Root KEK**: the top of the key hierarchy.\n" + // line 6, para term
		"\n" + // line 7
		"- **Foreign-rooted leaf**: a leaf under another root.\n" + // line 8, list term
		"\n" + // line 9
		"This is **bold** mid-sentence, not a term.\n" + // line 10, not a term
		"\n" + // line 11
		"_Avoid_: backdoor, standing key\n" + // line 12, italic lead, not a term
		"\n" + // line 13
		"**No colon here** and more text\n" // line 14, missing colon
	d := Parse("x.md", []byte(src))
	if len(d.Terms) != 2 {
		t.Fatalf("Terms = %+v, want exactly 2", d.Terms)
	}
	if d.Terms[0].Text != "Root KEK" || d.Terms[0].Line != 6 {
		t.Errorf("Terms[0] = %+v, want {Root KEK, 6}", d.Terms[0])
	}
	if d.Terms[1].Text != "Foreign-rooted leaf" || d.Terms[1].Line != 8 {
		t.Errorf("Terms[1] = %+v, want {Foreign-rooted leaf, 8}", d.Terms[1])
	}
}
