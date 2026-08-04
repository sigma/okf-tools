package bundle

import "testing"

// AnchorTarget is the single home of the glossary-anchor-vs-heading-anchor split
// the glossary rule, the heading-anchor rule, and the publisher all consume. This
// pins its contract directly, so the three consumers can trust one predicate.
func TestAnchorTarget(t *testing.T) {
	glossary := &Doc{Rel: "CONTEXT.md", Glossary: true}
	page := &Doc{Rel: "docs/b.md"}
	src := &Doc{Rel: "docs/a.md"}

	cases := []struct {
		name     string
		rl       ResolvedLink
		wantHost *Doc
		wantKind AnchorTargetKind
	}{
		{"no fragment", ResolvedLink{Class: ClassConcept, TargetDoc: page}, nil, NotAnchor},
		{"concept into glossary", ResolvedLink{Class: ClassConcept, Fragment: "term", TargetDoc: glossary}, glossary, GlossaryAnchor},
		{"concept into page", ResolvedLink{Class: ClassConcept, Fragment: "sec", TargetDoc: page}, page, HeadingAnchor},
		{"concept dangling target", ResolvedLink{Class: ClassConcept, Fragment: "sec", TargetDoc: nil}, nil, NotAnchor},
		{"same-file in glossary source", ResolvedLink{Class: ClassAnchor, Fragment: "term"}, src, HeadingAnchor}, // src is non-glossary here
		{"citation with fragment", ResolvedLink{Class: ClassCitation, Fragment: "x", TargetDoc: page}, nil, NotAnchor},
		{"external with fragment", ResolvedLink{Class: ClassExternal, Fragment: "x"}, nil, NotAnchor},
	}
	for _, c := range cases {
		host, kind := c.rl.AnchorTarget(src)
		if host != c.wantHost || kind != c.wantKind {
			t.Errorf("%s: AnchorTarget = (%v, %v), want (%v, %v)", c.name, host, kind, c.wantHost, c.wantKind)
		}
	}

	// A same-file anchor whose source IS a glossary resolves as a glossary anchor,
	// hosted by the source doc.
	if host, kind := (&ResolvedLink{Class: ClassAnchor, Fragment: "term"}).AnchorTarget(glossary); host != glossary || kind != GlossaryAnchor {
		t.Errorf("same-file anchor in glossary source = (%v, %v), want (%v, GlossaryAnchor)", host, kind, glossary)
	}
}
