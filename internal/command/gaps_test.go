package command

import "testing"

// fakeGapsRunner returns the same neighbor hits for any query: the seed self, a
// concept the seed links to, and a near-but-unlinked concept.
func fakeGapsRunner(dir string, args ...string) ([]byte, error) {
	return []byte(`[{"score":0.99,"file":"./seed.md"},{"score":0.70,"file":"./linked.md"},{"score":0.60,"file":"./near.md"}]`), nil
}

func TestGapsDirect(t *testing.T) {
	b := loadFixture(t, fixtureDir("gaps"))
	gapsRunner = fakeGapsRunner
	defer func() { gapsRunner = nil }()

	seed := b.ResolveWikilink("seed")
	if seed == nil {
		t.Fatal("seed concept not found")
	}
	res, err := computeGaps(b, seed, "direct", 10, 0.4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Direct) != 1 || res.Direct[0].Page != "near.md" {
		t.Fatalf("direct = %+v, want [near.md]", res.Direct)
	}
	linked := map[string]bool{}
	for _, n := range res.Neighbors {
		linked[n.Page] = n.Linked
	}
	if !linked["linked.md"] {
		t.Error("linked.md should be marked linked (seed links it)")
	}
	if linked["near.md"] {
		t.Error("near.md should be a gap, not linked")
	}
}

func TestGapsHoles(t *testing.T) {
	b := loadFixture(t, fixtureDir("gaps"))
	gapsRunner = fakeGapsRunner
	defer func() { gapsRunner = nil }()

	seed := b.ResolveWikilink("seed")
	res, err := computeGaps(b, seed, "neighborhood", 10, 0.4, nil)
	if err != nil {
		t.Fatal(err)
	}
	// linked.md and near.md are mutually near but unlinked -> one hole.
	if len(res.Holes) != 1 || res.Holes[0].A != "linked.md" || res.Holes[0].B != "near.md" {
		t.Fatalf("holes = %+v, want [linked.md -- near.md]", res.Holes)
	}
}

func TestGapsExcludeTypes(t *testing.T) {
	b := loadFixture(t, fixtureDir("gaps"))
	gapsRunner = fakeGapsRunner
	defer func() { gapsRunner = nil }()

	seed := b.ResolveWikilink("seed")
	// Excluding the Concept type drops every neighbor.
	res, err := computeGaps(b, seed, "direct", 10, 0.4, map[string]bool{"concept": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Neighbors) != 0 {
		t.Errorf("expected no neighbors when their type is excluded, got %+v", res.Neighbors)
	}
}
