package graph

import (
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
)

func rels(docs []*bundle.Doc) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Rel)
	}
	return out
}

func docsOf(rels ...string) []*bundle.Doc {
	out := make([]*bundle.Doc, 0, len(rels))
	for _, r := range rels {
		out = append(out, &bundle.Doc{Rel: r})
	}
	return out
}

// TestWithAreaRootsHoistsTheOverview pins that placement does not depend on the
// path sort: "concepts/ADR-index.md" sorts BEFORE "concepts/README.md", so a
// splice that merely re-sorted would put the overview second.
func TestWithAreaRootsHoistsTheOverview(t *testing.T) {
	docs := docsOf("CONTEXT.md", "concepts/ADR-index.md", "concepts/alpha.md")
	roots := docsOf("concepts/README.md")

	got := rels(withAreaRoots(docs, roots))
	want := []string{"CONTEXT.md", "concepts/README.md", "concepts/ADR-index.md", "concepts/alpha.md"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestWithAreaRootsAppendsALonelyRoot covers the area whose only page IS its
// README: there is nothing for it to precede, so it must still appear.
func TestWithAreaRootsAppendsALonelyRoot(t *testing.T) {
	docs := docsOf("CONTEXT.md")
	roots := docsOf("empty-area/README.md")

	got := rels(withAreaRoots(docs, roots))
	if len(got) != 2 || got[1] != "empty-area/README.md" {
		t.Errorf("a lone area root was dropped: %v", got)
	}
}

// TestWithAreaRootsIsANoopWithoutRoots keeps the un-narrowed path allocation-free
// in spirit: no roots, same slice contents.
func TestWithAreaRootsIsANoopWithoutRoots(t *testing.T) {
	docs := docsOf("a.md", "b.md")
	got := rels(withAreaRoots(docs, nil))
	if len(got) != 2 || got[0] != "a.md" || got[1] != "b.md" {
		t.Errorf("unexpected reorder: %v", got)
	}
}
