package graph

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
)

// --- fixtures ---------------------------------------------------------------

func loadBundle(t *testing.T, files map[string]string) *bundle.Bundle {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, cfgPath, err := bundle.Discover(dir, "", "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	b, err := bundle.Load(root, cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return b
}

func docByRel(t *testing.T, b *bundle.Bundle, rel string) *bundle.Doc {
	t.Helper()
	for _, d := range b.Docs {
		if d.Rel == rel {
			return d
		}
	}
	t.Fatalf("doc %q not in bundle", rel)
	return nil
}

// seed builds a CurrentState in which every rel in `unchanged` is an existing
// node whose scanned hash matches its source (so it hash-skips), plus any extra
// (rel -> stale hash) entries for existing-but-changed nodes and anchor names.
type seed struct {
	unchanged []string                   // existing + hash matches source
	changed   map[string]publish.Hash    // existing + hash differs
	anchors   map[publish.AnchorName]any // seeded anchor ids
}

func (s seed) build(t *testing.T, b *bundle.Bundle) *publish.CurrentState {
	t.Helper()
	nodeIDs := map[publish.SymbolicID]publish.BackendID{}
	hashes := map[publish.SymbolicID]publish.Hash{}
	propHashes := map[publish.SymbolicID]publish.Hash{}
	for _, rel := range s.unchanged {
		id := nodeRef(rel)
		nodeIDs[id] = publish.BackendID("be-" + rel)
		// An unchanged node hash-skips both arms, so seed both the content and the
		// property hash to their matching source values (the two-hash split).
		hashes[id] = ContentHash(docByRel(t, b, rel))
		propHashes[id] = PropertyHash(docByRel(t, b, rel))
	}
	for rel, h := range s.changed {
		id := nodeRef(rel)
		nodeIDs[id] = publish.BackendID("be-" + rel)
		// A stale content hash with no property hash re-asserts both arms — the
		// coupled "changed" case. Arm-independent cases seed via NewCurrentStateWithProps.
		hashes[id] = h
	}
	anchorIDs := map[publish.AnchorName]publish.BackendID{}
	for name := range s.anchors {
		anchorIDs[name] = publish.BackendID("be-anchor-" + string(name))
	}
	return publish.NewCurrentStateWithProps(nodeIDs, hashes, propHashes, anchorIDs)
}

// --- helpers over a Graph ---------------------------------------------------

func opFor(g *Graph, node publish.SymbolicID, kind OpKind) *Op {
	for _, op := range g.Ops {
		if op.Node == node && op.Kind == kind {
			return op
		}
	}
	return nil
}

func opKinds(g *Graph, node publish.SymbolicID) []OpKind {
	var out []OpKind
	for _, op := range g.Ops {
		if op.Node == node {
			out = append(out, op.Kind)
		}
	}
	return out
}

func edgesByCause(g *Graph, cause EdgeCause) []Edge {
	var out []Edge
	for _, e := range g.Edges {
		if e.Cause == cause {
			out = append(out, e)
		}
	}
	return out
}

func hasRef(refs []publish.SymbolicID, want publish.SymbolicID) bool {
	for _, r := range refs {
		if r == want {
			return true
		}
	}
	return false
}

func hasAnchor(anchors []publish.AnchorName, want publish.AnchorName) bool {
	for _, a := range anchors {
		if a == want {
			return true
		}
	}
	return false
}

func gen(t *testing.T, b *bundle.Bundle, cs *publish.CurrentState) *Graph {
	t.Helper()
	g, err := Generate(context.Background(), b, cs)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return g
}

// TestDiffArmsIndependent proves the two-hash split (#110 phase 2): with content
// and property hashes gated separately, a body-only edit emits only SetContent, a
// title/type-only edit only SetProperties, and a page unchanged on both stays
// hash-skipped — none of them coupled the way a single hash forced them to be.
func TestDiffArmsIndependent(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml": "",
		"index.md": "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"a.md":     "---\ntype: c\ntitle: A\n---\nBody.\n",
	})
	d := docByRel(t, b, "a.md")
	node := nodeRef("a.md")
	nodes := map[publish.SymbolicID]publish.BackendID{node: "be-a"}

	seedCS := func(content, prop publish.Hash) *publish.CurrentState {
		return publish.NewCurrentStateWithProps(nodes,
			map[publish.SymbolicID]publish.Hash{node: content},
			map[publish.SymbolicID]publish.Hash{node: prop}, nil)
	}
	content, prop := ContentHash(d), PropertyHash(d)

	cases := []struct {
		name string
		cs   *publish.CurrentState
		want []OpKind
	}{
		{"unchanged", seedCS(content, prop), nil},
		{"body-only edit", seedCS("stale", prop), []OpKind{SetContent}},
		{"property-only edit", seedCS(content, "stale"), []OpKind{SetProperties}},
		{"both edited", seedCS("stale", "stale"), []OpKind{SetProperties, SetContent}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := opKinds(gen(t, b, c.cs), node); !sameKinds(got, c.want) {
				t.Errorf("ops = %v, want %v", got, c.want)
			}
		})
	}
}

// --- the #162 worked example ------------------------------------------------

// workedExample reproduces #162's scenario: new 0002 links to unchanged 0001 and
// cites the glossary anchor root-kek; the glossary (CONTEXT.md) is being written.
func workedExample() map[string]string {
	return map[string]string{
		"okf.toml":          "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md":          "---\nokf_version: \"0.1\"\n---\n# ADRs\n",
		"docs/adr/index.md": "# ADRs\n",
		"docs/adr/0001.md":  "---\ntype: adr\ntitle: First\n---\nThe first decision.\n",
		"docs/adr/0002.md": "---\ntype: adr\ntitle: Second\nidentifier: \"0002\"\n---\n" +
			"Supersedes [ADR 0001](0001.md); protects the [root KEK](../../CONTEXT.md#root-kek).\n",
		"CONTEXT.md": "# Glossary\n\n**Root KEK**: the root key-encryption key.\n",
	}
}

func TestWorkedExample(t *testing.T) {
	b := loadBundle(t, workedExample())
	// 0001, both indexes unchanged; CONTEXT.md exists but changed (so its
	// SetContent is written and declares the anchor). 0002 is new (absent).
	cs := seed{
		unchanged: []string{"index.md", "docs/adr/index.md", "docs/adr/0001.md"},
		changed:   map[string]publish.Hash{"CONTEXT.md": "stale"},
	}.build(t, b)
	g := gen(t, b, cs)

	node0002 := nodeRef("docs/adr/0002.md")
	// New page → Create + SetProperties + SetContent, in that order.
	if got := opKinds(g, node0002); len(got) != 3 || got[0] != CreateNode || got[1] != SetProperties || got[2] != SetContent {
		t.Fatalf("0002 ops = %v, want [CreateNode SetProperties SetContent]", got)
	}
	// Its create is parented under the (existing) adr index — resolves from the
	// scan seed, so no parent edge.
	if create := opFor(g, node0002, CreateNode); create.Parent != nodeRef("docs/adr/index.md") {
		t.Errorf("0002 parent = %q, want the adr index", create.Parent)
	}
	// SetContent embeds both a node ref (to 0001) and the anchor ref.
	sc := opFor(g, node0002, SetContent)
	if !hasRef(sc.Refs, nodeRef("docs/adr/0001.md")) {
		t.Errorf("0002 refs = %v, missing node ref to 0001", sc.Refs)
	}
	if !hasRef(sc.Refs, publish.SymbolicID("anchor:glossary/root-kek")) {
		t.Errorf("0002 refs = %v, missing anchor:glossary/root-kek", sc.Refs)
	}

	// Unchanged docs emit nothing.
	for _, rel := range []string{"docs/adr/0001.md", "index.md", "docs/adr/index.md"} {
		if k := opKinds(g, nodeRef(rel)); len(k) != 0 {
			t.Errorf("%s should hash-skip, got ops %v", rel, k)
		}
	}
	// Changed glossary re-asserts and declares the anchor.
	if k := opKinds(g, nodeRef("CONTEXT.md")); len(k) != 2 || k[0] != SetProperties || k[1] != SetContent {
		t.Errorf("CONTEXT.md ops = %v, want [SetProperties SetContent]", k)
	}
	glossSC := opFor(g, nodeRef("CONTEXT.md"), SetContent)
	if !hasAnchor(glossSC.Anchors, "glossary/root-kek") {
		t.Errorf("glossary anchors = %v, want to include glossary/root-kek", glossSC.Anchors)
	}

	// Exactly one edge: the content-refs-anchor edge from the glossary body to
	// 0002's content. The node ref to 0001 resolves from the seed (no edge); the
	// parent resolves from the seed (no edge).
	if len(g.Edges) != 1 {
		t.Fatalf("edges = %d %+v, want exactly 1 (the anchor edge)", len(g.Edges), g.Edges)
	}
	e := g.Edges[0]
	if e.Cause != ContentRefsAnchor || e.From != glossSC || e.To != sc {
		t.Errorf("edge = %v (from %v to %v), want anchor edge glossary→0002", e.Cause, e.From.Node, e.To.Node)
	}
}

// --- publish scope: only areas.json-declared pages are published ------------

// scopedBundle has one content area (ideas), a glossary host (CONTEXT.md), and
// out-of-area trees (docs/agents, docs/runbooks) that the export contract
// excludes. An in-area page links both to another in-area page and to an
// out-of-area page, so link resolution must still see the whole tree.
func scopedBundle() map[string]string {
	return map[string]string{
		"okf.toml": "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"areas.json": `{
			"ideas":   {"directory": "ideas",   "type": "idea"},
			"context": {"file": "CONTEXT.md", "type": "context", "role": "glossary"}
		}`,
		"index.md":       "---\nokf_version: \"0.1\"\n---\nRoot index (out of every area).\n",
		"ideas/index.md": "Ideas area.\n",
		"ideas/a.md": "---\ntype: idea\ntitle: A\n---\n" +
			"See [B](b.md) and the [agent note](../docs/agents/note.md).\n",
		"ideas/b.md":          "---\ntype: idea\ntitle: B\n---\nThe second idea.\n",
		"CONTEXT.md":          "# Glossary\n\n**Root KEK**: the root key.\n",
		"docs/agents/note.md": "---\ntype: idea\ntitle: Note\n---\nAgent tooling, excluded from export.\n",
		"docs/runbooks/r.md":  "---\ntype: idea\ntitle: Runbook\n---\nA runbook, excluded from export.\n",
	}
}

func TestPublishScopedToAreas(t *testing.T) {
	b := loadBundle(t, scopedBundle())

	// The full tree is still loaded (link resolution needs it): the out-of-area
	// pages are in the bundle even though they will not be published, and each is
	// out of the publish scope.
	for _, rel := range []string{"docs/agents/note.md", "docs/runbooks/r.md", "index.md"} {
		docByRel(t, b, rel) // fatals if not loaded
		if b.InPublishScope(rel) {
			t.Errorf("%s should be outside the publish scope", rel)
		}
	}

	g := gen(t, b, publish.NewCurrentState(nil, nil, nil)) // all new

	// In-scope pages (area + glossary host) are published as new nodes.
	for _, rel := range []string{"ideas/index.md", "ideas/a.md", "ideas/b.md", "CONTEXT.md"} {
		if opFor(g, nodeRef(rel), CreateNode) == nil {
			t.Errorf("%s is in scope and should be published (CreateNode), got ops %v", rel, opKinds(g, nodeRef(rel)))
		}
	}

	// Out-of-area pages — and the root index, which sits under no declared area —
	// emit no ops at all: they are not published.
	for _, rel := range []string{"docs/agents/note.md", "docs/runbooks/r.md", "index.md"} {
		if k := opKinds(g, nodeRef(rel)); len(k) != 0 {
			t.Errorf("%s is out of export scope and must not be published, got ops %v", rel, k)
		}
	}

	// Cross-link resolution still works: the in-scope → in-scope link resolves to
	// a node ref, proving the narrowing gates only emission, not loading.
	sc := opFor(g, nodeRef("ideas/a.md"), SetContent)
	if sc == nil {
		t.Fatal("ideas/a.md should have a SetContent op")
	}
	if !hasRef(sc.Refs, nodeRef("ideas/b.md")) {
		t.Errorf("ideas/a.md refs = %v, missing in-scope node ref to ideas/b.md", sc.Refs)
	}
}

// TestPublishScopeReconcilesLeakedPages: a page that is out of scope but already
// present in the mirror (leaked by the pre-scoping publisher) reconciles to a
// deletion, since publish-set liveness no longer covers it.
func TestPublishScopeReconcilesLeakedPages(t *testing.T) {
	b := loadBundle(t, scopedBundle())
	// The mirror already holds the out-of-area page from a previous whole-tree run.
	cs := withVanished(t, publish.NewCurrentState(nil, nil, nil), "docs/agents/note.md")
	g := gen(t, b, cs)
	if opFor(g, nodeRef("docs/agents/note.md"), DeleteNode) == nil {
		t.Errorf("a leaked out-of-scope mirror page should reconcile to a DeleteNode")
	}
}

// --- diff → ops mapping -----------------------------------------------------

func TestDiffMapping(t *testing.T) {
	files := map[string]string{
		"okf.toml":  "",
		"index.md":  "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"new.md":    "---\ntype: c\n---\nBrand new.\n",
		"same.md":   "---\ntype: c\n---\nUntouched.\n",
		"edited.md": "---\ntype: c\n---\nEdited body.\n",
	}
	b := loadBundle(t, files)
	cs := seed{
		unchanged: []string{"index.md", "same.md"},
		changed:   map[string]publish.Hash{"edited.md": "stale"},
	}.build(t, b)
	// A scanned node with no source is an orphan.
	cs = withVanished(t, cs, "orphan.md")
	g := gen(t, b, cs)

	cases := []struct {
		rel  string
		want []OpKind
	}{
		{"new.md", []OpKind{CreateNode, SetProperties, SetContent}},
		{"edited.md", []OpKind{SetProperties, SetContent}},
		{"same.md", nil},
		{"index.md", nil},
	}
	for _, c := range cases {
		if got := opKinds(g, nodeRef(c.rel)); !sameKinds(got, c.want) {
			t.Errorf("%s ops = %v, want %v", c.rel, got, c.want)
		}
	}
	// Vanished → a single DeleteNode on the orphan.
	if op := opFor(g, nodeRef("orphan.md"), DeleteNode); op == nil {
		t.Errorf("orphan.md should produce a DeleteNode")
	}
}

// withVanished returns a CurrentState like cs but with extra scanned nodes that
// have no source — orphans awaiting deletion.
func withVanished(t *testing.T, cs *publish.CurrentState, rels ...string) *publish.CurrentState {
	t.Helper()
	nodeIDs := map[publish.SymbolicID]publish.BackendID{}
	hashes := map[publish.SymbolicID]publish.Hash{}
	propHashes := map[publish.SymbolicID]publish.Hash{}
	for id := range cs.Nodes() {
		be, _ := cs.NodeID(id)
		nodeIDs[id] = be
		if h, ok := cs.ContentHash(id); ok {
			hashes[id] = h
		}
		if h, ok := cs.PropertyHash(id); ok {
			propHashes[id] = h
		}
	}
	for _, rel := range rels {
		id := nodeRef(rel)
		nodeIDs[id] = publish.BackendID("be-" + rel)
		hashes[id] = "gone"
	}
	return publish.NewCurrentStateWithProps(nodeIDs, hashes, propHashes, nil)
}

// --- edge: parent-before-child ---------------------------------------------

func TestParentBeforeChildEdge(t *testing.T) {
	// Everything new (empty scan). Indexes carry no links so only parent edges arise.
	files := map[string]string{
		"okf.toml":     "",
		"index.md":     "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"sub/index.md": "Sub area.\n",
		"sub/a.md":     "---\ntype: c\n---\nLeaf.\n",
	}
	b := loadBundle(t, files)
	g := gen(t, b, publish.NewCurrentState(nil, nil, nil))

	parentEdges := edgesByCause(g, ParentBeforeChild)
	want := map[[2]publish.SymbolicID]bool{
		{nodeRef("index.md"), nodeRef("sub/index.md")}: false, // root → sub index
		{nodeRef("sub/index.md"), nodeRef("sub/a.md")}: false, // sub index → leaf
	}
	for _, e := range parentEdges {
		key := [2]publish.SymbolicID{e.From.Node, e.To.Node}
		if e.From.Kind != CreateNode || e.To.Kind != CreateNode {
			t.Errorf("parent edge %v→%v must connect two CreateNodes", e.From.Kind, e.To.Kind)
		}
		if _, ok := want[key]; !ok {
			t.Errorf("unexpected parent edge %s → %s", e.From.Node, e.To.Node)
			continue
		}
		want[key] = true
	}
	for key, seen := range want {
		if !seen {
			t.Errorf("missing parent edge %s → %s", key[0], key[1])
		}
	}
}

// --- edge: content-refs-node, incl. acyclic mutual links --------------------

func TestContentRefsNodeMutualAcyclic(t *testing.T) {
	files := map[string]string{
		"okf.toml": "",
		"index.md": "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"a.md":     "---\ntype: c\n---\nSee [B](b.md).\n",
		"b.md":     "---\ntype: c\n---\nSee [A](a.md).\n",
	}
	b := loadBundle(t, files)
	// index unchanged so its own listing links don't add noise; a and b new.
	cs := seed{unchanged: []string{"index.md"}}.build(t, b)
	g := gen(t, b, cs)

	nodeEdges := edgesByCause(g, ContentRefsNode)
	// Two edges: SetContent(a)→ waits on CreateNode(b) and vice versa. Every
	// content-refs-node edge must target a CreateNode (its existence), which is
	// what keeps mutual links acyclic.
	got := map[[2]publish.SymbolicID]bool{}
	for _, e := range nodeEdges {
		if e.From.Kind != CreateNode {
			t.Errorf("content-refs-node edge from %v, want it to target a CreateNode", e.From.Kind)
		}
		if e.To.Kind != SetContent {
			t.Errorf("content-refs-node edge to %v, want a SetContent", e.To.Kind)
		}
		got[[2]publish.SymbolicID{e.From.Node, e.To.Node}] = true
	}
	if !got[[2]publish.SymbolicID{nodeRef("b.md"), nodeRef("a.md")}] {
		t.Errorf("missing edge CreateNode(b) → SetContent(a); got %v", got)
	}
	if !got[[2]publish.SymbolicID{nodeRef("a.md"), nodeRef("b.md")}] {
		t.Errorf("missing edge CreateNode(a) → SetContent(b); got %v", got)
	}
	if cyclic(g) {
		t.Errorf("graph has a cycle; the Create/SetContent split should keep it acyclic")
	}
}

// --- edge: content-refs-anchor within the glossary is not a self-edge -------

func TestGlossarySelfReferenceNoSelfEdge(t *testing.T) {
	files := map[string]string{
		"okf.toml": "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md": "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		// One term cites another, in the same glossary body.
		"CONTEXT.md": "# Glossary\n\n**Root KEK**: protects the [DEK](CONTEXT.md#dek).\n\n**DEK**: a data key.\n",
	}
	b := loadBundle(t, files)
	g := gen(t, b, publish.NewCurrentState(nil, nil, nil)) // all new

	for _, e := range g.Edges {
		if e.From == e.To {
			t.Errorf("glossary intra-reference produced a self-edge %v", e.Cause)
		}
	}
	// The anchor ref within CONTEXT resolves inside its own SetContent, so it
	// yields no content-refs-anchor edge at all.
	if n := len(edgesByCause(g, ContentRefsAnchor)); n != 0 {
		t.Errorf("intra-glossary anchor should not edge; got %d anchor edges", n)
	}
}

// --- near-edgeless: unchanged bundle ---------------------------------------

func TestUnchangedBundleIsEdgeless(t *testing.T) {
	b := loadBundle(t, workedExample())
	all := []string{"index.md", "docs/adr/index.md", "docs/adr/0001.md", "docs/adr/0002.md", "CONTEXT.md"}
	cs := seed{
		unchanged: all,
		anchors:   map[publish.AnchorName]any{"glossary/root-kek": nil},
	}.build(t, b)
	g := gen(t, b, cs)

	if len(g.Ops) != 0 {
		t.Errorf("unchanged bundle emitted %d ops, want 0: %+v", len(g.Ops), g.Ops)
	}
	if len(g.Edges) != 0 {
		t.Errorf("unchanged bundle emitted %d edges, want 0", len(g.Edges))
	}
}

// --- orphan subtree collapses to one DeleteNode on the root -----------------

func TestOrphanSubtreeRoot(t *testing.T) {
	files := map[string]string{
		"okf.toml": "",
		"index.md": "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"live.md":  "---\ntype: c\n---\nStill here.\n",
	}
	b := loadBundle(t, files)
	cs := seed{unchanged: []string{"index.md", "live.md"}}.build(t, b)
	// A whole vanished cluster plus a standalone orphan.
	cs = withVanished(t, cs, "dead/index.md", "dead/child.md", "gone.md")
	g := gen(t, b, cs)

	var deletes []publish.SymbolicID
	for _, op := range g.Ops {
		if op.Kind == DeleteNode {
			deletes = append(deletes, op.Node)
		}
	}
	want := map[publish.SymbolicID]bool{nodeRef("dead/index.md"): true, nodeRef("gone.md"): true}
	if len(deletes) != len(want) {
		t.Fatalf("deletes = %v, want the two subtree roots %v", deletes, want)
	}
	for _, d := range deletes {
		if !want[d] {
			t.Errorf("unexpected DeleteNode %s (a covered subpage should be skipped)", d)
		}
	}
}

// --- neutral tree carries first-class Ref nodes -----------------------------

func TestNeutralTreeHasRefNodes(t *testing.T) {
	b := loadBundle(t, workedExample())
	cs := seed{
		unchanged: []string{"index.md", "docs/adr/index.md", "docs/adr/0001.md"},
		changed:   map[string]publish.Hash{"CONTEXT.md": "stale"},
	}.build(t, b)
	g := gen(t, b, cs)

	sc := opFor(g, nodeRef("docs/adr/0002.md"), SetContent)
	if sc.Doc == nil || len(sc.Doc.Blocks) == 0 {
		t.Fatalf("SetContent carries no document tree")
	}
	if sc.Doc.Group != publish.GroupKey("node:docs/adr/0002.md") {
		t.Errorf("document group = %q, want the target node", sc.Doc.Group)
	}
	var refIDs []publish.SymbolicID
	for _, blk := range sc.Doc.Blocks {
		bc, ok := blk.Content.(BlockContent)
		if !ok {
			t.Fatalf("block content is %T, want BlockContent", blk.Content)
		}
		for _, in := range bc.Inlines {
			if in.Ref != nil {
				refIDs = append(refIDs, in.Ref.ID)
			}
		}
	}
	if !hasRef(refIDs, nodeRef("docs/adr/0001.md")) || !hasRef(refIDs, publish.SymbolicID("anchor:glossary/root-kek")) {
		t.Errorf("inline Ref ids = %v, want both node:0001 and anchor:glossary/root-kek", refIDs)
	}
}

// --- external links and images are not refs --------------------------------

func TestNonConceptLinksAreNotRefs(t *testing.T) {
	files := map[string]string{
		"okf.toml": "",
		"index.md": "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"p.md":     "---\ntype: c\n---\nSee [site](https://example.com) and ![pic](img.png).\n",
	}
	b := loadBundle(t, files)
	g := gen(t, b, publish.NewCurrentState(nil, nil, nil))
	sc := opFor(g, nodeRef("p.md"), SetContent)
	if len(sc.Refs) != 0 {
		t.Errorf("external link + image produced refs %v, want none", sc.Refs)
	}
}

// --- linked images keep the link ordinal aligned ---------------------------

// A linked image ([![alt](img)](target)) is two link-like nodes to the parser
// (the link and the nested image) but renders as one. If generation miscounts, a
// later concept link resolves against the wrong ResolvedLink. Both refs must
// survive.
func TestLinkedImageOrdinalStaysAligned(t *testing.T) {
	files := map[string]string{
		"okf.toml": "",
		"index.md": "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"a.md":     "---\ntype: c\n---\nA.\n",
		"b.md":     "---\ntype: c\n---\nB.\n",
		"p.md":     "---\ntype: c\n---\nBanner [![diagram](d.png)](a.md) then see [B](b.md).\n",
	}
	b := loadBundle(t, files)
	cs := seed{unchanged: []string{"index.md", "a.md", "b.md"}}.build(t, b)
	g := gen(t, b, cs)
	sc := opFor(g, nodeRef("p.md"), SetContent)
	if !hasRef(sc.Refs, nodeRef("a.md")) || !hasRef(sc.Refs, nodeRef("b.md")) {
		t.Errorf("p refs = %v, want both node:a.md (behind the linked image) and node:b.md", sc.Refs)
	}
}

// --- change detection covers frontmatter ------------------------------------

// A property-only edit (same body, different frontmatter) must change the hash so
// the "changed" arm — SetProperties + SetContent — can fire.
func TestContentHashCoversFrontmatter(t *testing.T) {
	files := map[string]string{
		"okf.toml": "",
		"index.md": "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"x.md":     "---\ntype: c\ntitle: Old\n---\nIdentical body.\n",
		"y.md":     "---\ntype: c\ntitle: New\n---\nIdentical body.\n",
	}
	b := loadBundle(t, files)
	if ContentHash(docByRel(t, b, "x.md")) == ContentHash(docByRel(t, b, "y.md")) {
		t.Errorf("frontmatter-only difference must change the content hash")
	}
}

// --- determinism ------------------------------------------------------------

func TestDeterministicAcrossRuns(t *testing.T) {
	b := loadBundle(t, workedExample())
	cs := func() *publish.CurrentState {
		return seed{
			unchanged: []string{"index.md", "docs/adr/index.md", "docs/adr/0001.md"},
			changed:   map[string]publish.Hash{"CONTEXT.md": "stale"},
		}.build(t, b)
	}
	g1 := gen(t, b, cs())
	g2 := gen(t, b, cs())
	if len(g1.Ops) != len(g2.Ops) {
		t.Fatalf("op count varies: %d vs %d", len(g1.Ops), len(g2.Ops))
	}
	for i := range g1.Ops {
		if g1.Ops[i].Node != g2.Ops[i].Node || g1.Ops[i].Kind != g2.Ops[i].Kind {
			t.Fatalf("op %d differs: %v/%v vs %v/%v", i, g1.Ops[i].Kind, g1.Ops[i].Node, g2.Ops[i].Kind, g2.Ops[i].Node)
		}
	}
	if len(g1.Edges) != len(g2.Edges) {
		t.Fatalf("edge count varies: %d vs %d", len(g1.Edges), len(g2.Edges))
	}
	for i := range g1.Edges {
		if g1.Edges[i].From.Node != g2.Edges[i].From.Node || g1.Edges[i].To.Node != g2.Edges[i].To.Node {
			t.Fatalf("edge %d differs across runs", i)
		}
	}
}

// --- test utilities ---------------------------------------------------------

func sameKinds(a, b []OpKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// cyclic reports whether g.Edges contain a directed cycle (Kahn's algorithm over
// the distinct op pointers touched by edges).
func cyclic(g *Graph) bool {
	indeg := map[*Op]int{}
	adj := map[*Op][]*Op{}
	nodes := map[*Op]bool{}
	for _, e := range g.Edges {
		adj[e.From] = append(adj[e.From], e.To)
		indeg[e.To]++
		nodes[e.From] = true
		nodes[e.To] = true
	}
	var queue []*Op
	for n := range nodes {
		if indeg[n] == 0 {
			queue = append(queue, n)
		}
	}
	visited := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		visited++
		for _, m := range adj[n] {
			indeg[m]--
			if indeg[m] == 0 {
				queue = append(queue, m)
			}
		}
	}
	return visited != len(nodes)
}

// --- README-clustered subpage nesting (#90) ---------------------------------

// A cluster whose entry point is README.md (no index.md) must parent its sibling
// pages under that README, and the area-root README must not publish as a row.
func TestReadmeClusterNesting(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml": "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"areas.json": `{
			"specs":   {"directory": "specs", "type": "spec"},
			"context": {"file": "CONTEXT.md", "type": "context", "role": "glossary"}
		}`,
		"index.md":                "---\nokf_version: \"0.1\"\n---\nRoot (out of every area).\n",
		"specs/README.md":         "# Specs\nArea landing.\n",
		"specs/top.md":            "---\ntype: spec\ntitle: Top\n---\nA top-level spec.\n",
		"specs/cluster/README.md": "---\ntype: spec\ntitle: Cluster\n---\nCluster entry point.\n",
		"specs/cluster/a.md":      "---\ntype: spec\ntitle: A\n---\nChild A.\n",
		"specs/cluster/b.md":      "---\ntype: spec\ntitle: B\n---\nChild B.\n",
		"CONTEXT.md":              "# Glossary\n",
	})

	g := gen(t, b, nil) // fresh scan: every in-scope page is a CreateNode

	// The area-root README is not a row.
	if op := opFor(g, nodeRef("specs/README.md"), CreateNode); op != nil {
		t.Errorf("area-root README published as a node (parent %q); it must be skipped", op.Parent)
	}

	// Cluster siblings nest under the cluster README.
	wantParent := map[string]publish.SymbolicID{
		"specs/cluster/a.md":      nodeRef("specs/cluster/README.md"),
		"specs/cluster/b.md":      nodeRef("specs/cluster/README.md"),
		"specs/cluster/README.md": "", // the cluster page is itself a top-level row of the area DB
		"specs/top.md":            "", // a plain area page is top-level
	}
	for rel, want := range wantParent {
		op := opFor(g, nodeRef(rel), CreateNode)
		if op == nil {
			t.Fatalf("no CreateNode for %q", rel)
		}
		if op.Parent != want {
			t.Errorf("parent(%q) = %q, want %q", rel, op.Parent, want)
		}
	}

	// The parent-before-child edge for the cluster is present (README created this
	// run, so its children depend on it).
	parentEdges := edgesByCause(g, ParentBeforeChild)
	want := map[[2]publish.SymbolicID]bool{
		{nodeRef("specs/cluster/README.md"), nodeRef("specs/cluster/a.md")}: false,
		{nodeRef("specs/cluster/README.md"), nodeRef("specs/cluster/b.md")}: false,
	}
	for _, e := range parentEdges {
		key := [2]publish.SymbolicID{e.From.Node, e.To.Node}
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, seen := range want {
		if !seen {
			t.Errorf("missing parent edge %s → %s", key[0], key[1])
		}
	}
}

// index.md still outranks a sibling README.md as a directory's index, so a
// mixed directory nests under index.md (README recognition is a fallback only).
func TestIndexMdOutranksReadme(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml":      "",
		"index.md":      "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"sub/index.md":  "# Sub\n",
		"sub/README.md": "# Sub readme\n",
		"sub/leaf.md":   "---\ntitle: Leaf\n---\nLeaf.\n",
	})
	g := gen(t, b, nil)
	if op := opFor(g, nodeRef("sub/leaf.md"), CreateNode); op == nil || op.Parent != nodeRef("sub/index.md") {
		var got publish.SymbolicID
		if op != nil {
			got = op.Parent
		}
		t.Errorf("parent(sub/leaf.md) = %q, want the index.md (index.md outranks README.md)", got)
	}
}
