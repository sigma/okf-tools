package parser

import (
	"testing"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// IsLinkLike is the single predicate defining which inline nodes become Links.
// The publish graph re-walks the same AST gated by this predicate to attach
// resolution, so it must match d.Links exactly — one true per collected Link,
// and false for other node kinds. If they ever drift, graph resolution desyncs.
func TestIsLinkLikeMatchesLinkCollection(t *testing.T) {
	// One of each link-like kind (markdown link, image, angle-bracket autolink,
	// wikilink) plus non-link nodes (heading, plain text).
	d := Parse("x.md", []byte("---\n---\n# Heading\n\nText [md](a.md), ![img](i.png), <https://x.example>, and [[wiki]].\n"))

	root := Markdown().Parser().Parse(text.NewReader([]byte(d.Body)))
	linkLike, sawHeading := 0, false
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch {
		case IsLinkLike(n):
			linkLike++
		case func() bool { _, ok := n.(*ast.Heading); return ok }():
			sawHeading = true
			if IsLinkLike(n) {
				t.Errorf("IsLinkLike(*ast.Heading) = true, want false")
			}
		}
		return ast.WalkContinue, nil
	})

	if linkLike != len(d.Links) {
		t.Errorf("IsLinkLike matched %d nodes but parser collected %d Links — predicate and d.Links must agree", linkLike, len(d.Links))
	}
	if len(d.Links) != 4 {
		t.Errorf("want 4 links (link, image, autolink, wikilink), got %d: %+v", len(d.Links), d.Links)
	}
	if !sawHeading {
		t.Errorf("expected a Heading node in the fixture")
	}
}
