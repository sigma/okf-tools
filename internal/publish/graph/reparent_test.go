package graph

import (
	"fmt"
	"slices"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/bundle/bundletest"
	"github.com/sigma/okf-tools/internal/publish"
)

// ownedSeed builds a CurrentState whose every node is unchanged (both hashes match
// the source, or a stale one for a node absent from the source) and RECORDED where
// owners says: "" for a node that is its own row, else the symbolic id of the row
// whose subtree map holds it. It is the shape a scan of a mirror published from an
// earlier source tree produces.
func ownedSeed(t *testing.T, b *bundle.Bundle, owners map[string]string) *publish.CurrentState {
	t.Helper()
	nodeIDs := map[publish.SymbolicID]publish.BackendID{}
	hashes := map[publish.SymbolicID]publish.Hash{}
	propHashes := map[publish.SymbolicID]publish.Hash{}
	owned := map[publish.SymbolicID]publish.SymbolicID{}
	for rel, owner := range owners {
		id := nodeRef(rel)
		nodeIDs[id] = publish.BackendID("be-" + rel)
		hashes[id] = "gone"
		for _, d := range b.Docs {
			if d.Rel == rel {
				hashes[id] = ContentHash(d)
				propHashes[id] = PropertyHash(d)
			}
		}
		if owner == "" {
			owned[id] = ""
		} else {
			owned[id] = nodeRef(owner)
		}
	}
	return publish.NewCurrentStateWithOwners(nodeIDs, hashes, propHashes, nil, owned)
}

// areaDocs declares one content area, so a README inside it is a cluster index and
// a page directly under the area is a row — the two placements a re-parent moves
// between. The root index.md stays out of every area and parents nothing.
const areaDocs = `{"docs": {"directory": "docs", "type": "doc"}}`

func deleteOps(g *Graph) []*Op {
	var out []*Op
	for _, op := range g.Ops {
		if op.Kind == DeleteNode {
			out = append(out, op)
		}
	}
	return out
}

// describe renders delete ops as "target[covers]" for failure messages.
func describe(ops []*Op) []string {
	var out []string
	for _, op := range ops {
		out = append(out, fmt.Sprintf("%s%v", op.Node, op.Covers))
	}
	return out
}

// A README appearing over already-published rows turns them into a cluster. Each
// row's stored owner (itself) now differs from its expected one (the README), so
// each is re-parented: its old row is archived — addressed by its backend id, since
// the path now names the NEW page — and a new page is created under the README.
// The unchanged sibling migrates too; an owner change is enough (sigma/okf-tools#209).
func TestReparentWhenReadmeAppears(t *testing.T) {
	b := bundletest.Load(t, map[string]string{
		"okf.toml":           "",
		"areas.json":         areaDocs,
		"index.md":           "---\nokf_version: \"0.1\"\n---\nRoot (out of every area).\n",
		"docs/README.md":     "# Docs\nArea landing.\n",
		"docs/top.md":        "---\ntitle: Top\n---\nA row before and after.\n",
		"docs/dir/README.md": "---\ntitle: Cluster\n---\nEntry point.\n",
		"docs/dir/a.md":      "---\ntitle: A\n---\nA.\n",
		"docs/dir/b.md":      "---\ntitle: B\n---\nB.\n",
	})
	cs := ownedSeed(t, b, map[string]string{"docs/top.md": "", "docs/dir/a.md": "", "docs/dir/b.md": ""})
	g := gen(t, b, cs)

	for _, rel := range []string{"docs/dir/a.md", "docs/dir/b.md"} {
		if got := opKinds(g, nodeRef(rel)); !slices.Equal(got, []OpKind{CreateNode, SetProperties, SetContent}) {
			t.Errorf("%s ops = %v, want a full create under the new parent", rel, got)
		}
		if op := opFor(g, nodeRef(rel), CreateNode); op != nil && op.Parent != nodeRef("docs/dir/README.md") {
			t.Errorf("%s re-created with parent %q, want the README", rel, op.Parent)
		}
		del := opFor(g, publish.UnclaimedRef(publish.BackendID("be-"+rel)), DeleteNode)
		if del == nil {
			t.Fatalf("%s: no DeleteNode addressing the old row by its backend id; deletes = %v", rel, describe(deleteOps(g)))
		}
		if !slices.Equal(del.Covers, []publish.SymbolicID{nodeRef(rel)}) {
			t.Errorf("%s: old row's delete covers %v, want the path it is giving up", rel, del.Covers)
		}
	}
	if got := opKinds(g, nodeRef("docs/top.md")); len(got) != 0 {
		t.Errorf("top.md (row before and after) should hash-skip, got %v", got)
	}
	if n := len(deleteOps(g)); n != 2 {
		t.Errorf("%d deletes, want exactly the two re-parented rows", n)
	}
	// A re-parent is not a reclaim: the old row was recorded, and the CLI must not
	// report it as debris of an interrupted run.
	if n := g.UnclaimedDeletes(); n != 0 {
		t.Errorf("UnclaimedDeletes = %d, want 0", n)
	}
}

// The reverse: a README removed. The README row vanishes and its subpages, whose
// stored owner is the README, are expected as rows. The README's single archive
// takes its subpages with it, so they are COVERED by its delete rather than
// archived twice, and each is re-created as a row.
func TestReparentWhenReadmeVanishes(t *testing.T) {
	b := bundletest.Load(t, map[string]string{
		"okf.toml":       "",
		"areas.json":     areaDocs,
		"index.md":       "---\nokf_version: \"0.1\"\n---\nRoot (out of every area).\n",
		"docs/README.md": "# Docs\nArea landing.\n",
		"docs/dir/a.md":  "---\ntitle: A\n---\nA.\n",
		"docs/dir/b.md":  "---\ntitle: B\n---\nB.\n",
	})
	cs := ownedSeed(t, b, map[string]string{
		"docs/dir/README.md": "", "docs/dir/a.md": "docs/dir/README.md", "docs/dir/b.md": "docs/dir/README.md",
	})
	g := gen(t, b, cs)

	dels := deleteOps(g)
	if len(dels) != 1 || dels[0].Node != nodeRef("docs/dir/README.md") {
		t.Fatalf("deletes = %v, want only the README row (its subpages go with it)", describe(dels))
	}
	if want := []publish.SymbolicID{nodeRef("docs/dir/a.md"), nodeRef("docs/dir/b.md")}; !slices.Equal(dels[0].Covers, want) {
		t.Errorf("README delete covers %v, want %v", dels[0].Covers, want)
	}
	for _, rel := range []string{"docs/dir/a.md", "docs/dir/b.md"} {
		op := opFor(g, nodeRef(rel), CreateNode)
		if op == nil {
			t.Fatalf("%s: no CreateNode; ops = %v", rel, opKinds(g, nodeRef(rel)))
		}
		if op.Parent != "" {
			t.Errorf("%s re-created with parent %q, want a top-level row", rel, op.Parent)
		}
	}
}

// A subpage moving between two clusters is re-parented under the new one; a
// nested cluster's leaf still records to the nearest ROW (#141), which is what the
// op's Owner says.
func TestReparentBetweenClusters(t *testing.T) {
	b := bundletest.Load(t, map[string]string{
		"okf.toml":              "",
		"areas.json":            areaDocs,
		"index.md":              "---\nokf_version: \"0.1\"\n---\nRoot (out of every area).\n",
		"docs/README.md":        "# Docs\nArea landing.\n",
		"docs/x/README.md":      "# X\n",
		"docs/y/README.md":      "# Y\n",
		"docs/y/deep/README.md": "# Deep\n",
		"docs/y/deep/leaf.md":   "---\ntitle: Leaf\n---\nLeaf.\n",
	})
	// leaf.md used to live under x (a subpage recorded on x's row); the source now
	// has it under y/deep, a nested index whose row is y.
	cs := ownedSeed(t, b, map[string]string{
		"docs/x/README.md": "", "docs/y/README.md": "", "docs/y/deep/README.md": "docs/y/README.md",
		"docs/y/deep/leaf.md": "docs/x/README.md",
	})
	g := gen(t, b, cs)

	op := opFor(g, nodeRef("docs/y/deep/leaf.md"), CreateNode)
	if op == nil {
		t.Fatalf("leaf not re-created; ops = %v", opKinds(g, nodeRef("docs/y/deep/leaf.md")))
	}
	if op.Parent != nodeRef("docs/y/deep/README.md") || op.Owner != nodeRef("docs/y/README.md") {
		t.Errorf("leaf parent/owner = %q/%q, want the nested index / the y row", op.Parent, op.Owner)
	}
	if del := opFor(g, publish.UnclaimedRef("be-docs/y/deep/leaf.md"), DeleteNode); del == nil {
		t.Errorf("old leaf page not archived; deletes = %v", describe(deleteOps(g)))
	}
	if got := opKinds(g, nodeRef("docs/y/deep/README.md")); len(got) != 0 {
		t.Errorf("an index whose owner is unchanged should hash-skip, got %v", got)
	}
}

// A whole re-parented subtree is archived once: a nested index that moves takes
// its children with it, so the children are covered by its delete and re-created
// under the new page, never archived on their own.
func TestReparentSubtreeArchivesOnce(t *testing.T) {
	b := bundletest.Load(t, map[string]string{
		"okf.toml":              "",
		"areas.json":            areaDocs,
		"index.md":              "---\nokf_version: \"0.1\"\n---\nRoot (out of every area).\n",
		"docs/README.md":        "# Docs\nArea landing.\n",
		"docs/top/README.md":    "# Top\n",
		"docs/top/in/README.md": "# In\n",
		"docs/top/in/leaf.md":   "---\ntitle: Leaf\n---\nLeaf.\n",
	})
	// top/README.md is new: top/in was a top-level cluster (its README a row).
	cs := ownedSeed(t, b, map[string]string{
		"docs/top/in/README.md": "", "docs/top/in/leaf.md": "docs/top/in/README.md",
	})
	g := gen(t, b, cs)

	dels := deleteOps(g)
	if len(dels) != 1 || dels[0].Node != publish.UnclaimedRef("be-docs/top/in/README.md") {
		t.Fatalf("deletes = %v, want one archive of the old top/in row", describe(dels))
	}
	if want := []publish.SymbolicID{nodeRef("docs/top/in/README.md"), nodeRef("docs/top/in/leaf.md")}; !slices.Equal(dels[0].Covers, want) {
		t.Errorf("covers %v, want %v", dels[0].Covers, want)
	}
	for _, rel := range []string{"docs/top/in/README.md", "docs/top/in/leaf.md"} {
		if opFor(g, nodeRef(rel), CreateNode) == nil {
			t.Errorf("%s not re-created", rel)
		}
	}
}

// A scanner that supplies no owner fact cannot detect a re-parent, and must not
// invent one: an existing node whose parent changed is still an in-place update —
// exactly what every backend did before the fact existed.
func TestReparentNeedsStoredOwner(t *testing.T) {
	b := bundletest.Load(t, map[string]string{
		"okf.toml":           "",
		"areas.json":         areaDocs,
		"index.md":           "---\nokf_version: \"0.1\"\n---\nRoot (out of every area).\n",
		"docs/README.md":     "# Docs\nArea landing.\n",
		"docs/dir/README.md": "# Cluster\n",
		"docs/dir/a.md":      "---\ntitle: A\n---\nA.\n",
	})
	cs := seed{unchanged: []string{"docs/dir/README.md", "docs/dir/a.md"}}.build(t, b)
	g := gen(t, b, cs)

	if got := opKinds(g, nodeRef("docs/dir/a.md")); len(got) != 0 {
		t.Errorf("without a stored owner a.md should hash-skip, got %v", got)
	}
	if n := len(deleteOps(g)); n != 0 {
		t.Errorf("%d deletes, want none", n)
	}
}
