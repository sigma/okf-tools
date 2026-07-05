package parser

import "testing"

func TestBOMStripped(t *testing.T) {
	d := Parse("x.md", []byte("\ufeff---\ntype: T\n---\n# H\n"))
	if !d.HasFrontmatter() {
		t.Fatal("a leading BOM should be stripped so frontmatter parses")
	}
}

func TestCRLFFrontmatter(t *testing.T) {
	d := Parse("x.md", []byte("---\r\ntype: T\r\n---\r\nBody [a](a.md)\r\n"))
	if !d.HasFrontmatter() || d.Frontmatter["type"] != "T" {
		t.Fatalf("CRLF frontmatter should parse: err=%v fm=%v", d.ParseErr, d.Frontmatter)
	}
	if len(d.Links) != 1 || d.Links[0].Target != "a.md" {
		t.Errorf("CRLF body link not found: %+v", d.Links)
	}
}

func TestFrontmatterOnlyNoBody(t *testing.T) {
	d := Parse("x.md", []byte("---\ntype: T\n---\n"))
	if !d.HasFrontmatter() {
		t.Fatal("frontmatter-only doc should parse")
	}
	if len(d.Links) != 0 || len(d.Headings) != 0 {
		t.Errorf("no body expected, got links=%v headings=%v", d.Links, d.Headings)
	}
}

func TestImageVsLink(t *testing.T) {
	d := Parse("x.md", []byte("![alt](img.png) and [text](page.md)\n"))
	var img, link *Link
	for i := range d.Links {
		if d.Links[i].Image {
			img = &d.Links[i]
		} else {
			link = &d.Links[i]
		}
	}
	if img == nil || img.Target != "img.png" {
		t.Errorf("image not detected: %+v", d.Links)
	}
	if link == nil || link.Target != "page.md" {
		t.Errorf("link not detected: %+v", d.Links)
	}
}

func TestListItems(t *testing.T) {
	d := Parse("x.md", []byte("# H\n\n* [a](a.md)\n* plain text\n"))
	if len(d.ListItems) != 2 {
		t.Fatalf("got %d list items, want 2", len(d.ListItems))
	}
	withLink, without := 0, 0
	for _, li := range d.ListItems {
		if li.HasLink {
			withLink++
		} else {
			without++
		}
	}
	if withLink != 1 || without != 1 {
		t.Errorf("HasLink split = %d/%d, want 1/1", withLink, without)
	}
}
