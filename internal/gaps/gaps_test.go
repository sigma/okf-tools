package gaps

import (
	"path/filepath"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
)

func fixtureDir(name string) string { return filepath.Join("..", "..", "testdata", name) }

func loadFixture(t *testing.T, dir string) *bundle.Bundle {
	t.Helper()
	root, cfg, err := bundle.Discover(dir, "", "")
	if err != nil {
		t.Fatalf("discover %s: %v", dir, err)
	}
	b, err := bundle.Load(root, cfg)
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	return b
}

// fakeRunner returns the same neighbor hits for any query: the seed self, a
// concept the seed links to, and a near-but-unlinked concept. Injected through
// Params.Runner — no package global to reset.
func fakeRunner(dir string, args ...string) ([]byte, error) {
	return []byte(`[{"score":0.99,"file":"./seed.md"},{"score":0.70,"file":"./linked.md"},{"score":0.60,"file":"./near.md"}]`), nil
}

func TestFindDirect(t *testing.T) {
	b := loadFixture(t, fixtureDir("gaps"))
	seed := b.ResolveWikilink("seed")
	if seed == nil {
		t.Fatal("seed concept not found")
	}
	res, err := Find(b, seed, Params{Depth: "direct", Top: 10, MinSim: 0.4, Runner: fakeRunner})
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

func TestFindHoles(t *testing.T) {
	b := loadFixture(t, fixtureDir("gaps"))
	seed := b.ResolveWikilink("seed")
	res, err := Find(b, seed, Params{Depth: "neighborhood", Top: 10, MinSim: 0.4, Runner: fakeRunner})
	if err != nil {
		t.Fatal(err)
	}
	// linked.md and near.md are mutually near but unlinked -> one hole.
	if len(res.Holes) != 1 || res.Holes[0].A != "linked.md" || res.Holes[0].B != "near.md" {
		t.Fatalf("holes = %+v, want [linked.md -- near.md]", res.Holes)
	}
}

func TestFindExcludeTypes(t *testing.T) {
	b := loadFixture(t, fixtureDir("gaps"))
	seed := b.ResolveWikilink("seed")
	// Excluding the Concept type drops every neighbor.
	res, err := Find(b, seed, Params{Depth: "direct", Top: 10, MinSim: 0.4, Exclude: map[string]bool{"concept": true}, Runner: fakeRunner})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Neighbors) != 0 {
		t.Errorf("expected no neighbors when their type is excluded, got %+v", res.Neighbors)
	}
}

// TestSeedLinksPopulated confirms Result carries the seed's existing links so the
// renderer needs only the Result — the self-contained-output invariant.
func TestSeedLinksPopulated(t *testing.T) {
	b := loadFixture(t, fixtureDir("gaps"))
	seed := b.ResolveWikilink("seed")
	res, err := Find(b, seed, Params{Depth: "direct", Top: 10, MinSim: 0.4, Runner: fakeRunner})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range res.SeedLinks {
		if l == "linked.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("SeedLinks = %v, want it to include linked.md", res.SeedLinks)
	}
}
