package graph

import (
	"testing"
)

// A linked image [![alt](img.png)](a.md) records TWO link-like nodes in the
// parser's depth-first walk — the outer concept Link and the nested Image — but
// the graph renders only the outer link and never descends into its children, so
// the nested Image node sits unread in the resolution map. A following [B](b.md)
// must still resolve to node:b.md, proving resolution is keyed by node identity
// rather than a positional ordinal a nested node could throw off (the bookkeeping
// the deleted consumeNested did by hand).
func TestNestedLinkLikeDoesNotDesyncResolution(t *testing.T) {
	files := map[string]string{
		"okf.toml": "",
		"index.md": "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"a.md":     "---\ntype: c\n---\nA.\n",
		"b.md":     "---\ntype: c\n---\nB.\n",
		"p.md":     "---\ntype: c\n---\n[![alt](img.png)](a.md)\n\nThen see [B](b.md).\n",
	}
	b := loadBundle(t, files)
	cs := seed{unchanged: []string{"index.md", "a.md", "b.md"}}.build(t, b)
	g := gen(t, b, cs)
	sc := opFor(g, nodeRef("p.md"), SetContent)
	if !hasRef(sc.Refs, nodeRef("a.md")) || !hasRef(sc.Refs, nodeRef("b.md")) {
		t.Errorf("p refs = %v, want both node:a.md (the linked image) and node:b.md", sc.Refs)
	}
}
