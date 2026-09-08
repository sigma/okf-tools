package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
)

// TestContentHashCoversTheRenderer is the #183 regression. The hash covered the
// SOURCE only, so a fix to how a bundle RENDERS left every hash identical: change
// detection skipped every node and the mirror kept serving the old — in the
// #181/#182 cases, corrupt — output, while the run reported "0 transaction(s)"
// and success.
func TestContentHashCoversTheRenderer(t *testing.T) {
	b := loadBundle(t, workedExample())
	d := docByRel(t, b, "docs/adr/0001.md")

	bare := sha256.Sum256([]byte(d.Content))
	if string(ContentHash(d)) == hex.EncodeToString(bare[:]) {
		t.Error("ContentHash is a plain hash of the source: a renderer change cannot move it")
	}
}

// The version is what moves the hash, so a bump re-renders every page exactly
// once — the property the whole mechanism rests on.
func TestRendererVersionMovesEveryHash(t *testing.T) {
	b := loadBundle(t, workedExample())
	d := docByRel(t, b, "docs/adr/0001.md")

	if contentHashAt(RendererVersion, d) != ContentHash(d) {
		t.Fatal("ContentHash does not hash at the current renderer version")
	}
	if contentHashAt(RendererVersion+1, d) == ContentHash(d) {
		t.Error("bumping the renderer version left the hash unchanged")
	}
}

// Same version, same source, same hash — across two independent loads of the
// bundle, which is what a re-run actually is. A steady-state re-run must still be
// a near-noop, and that is what makes the version mix affordable.
func TestRendererVersionIsStableAcrossRuns(t *testing.T) {
	first := ContentHash(docByRel(t, loadBundle(t, workedExample()), "docs/adr/0001.md"))
	second := ContentHash(docByRel(t, loadBundle(t, workedExample()), "docs/adr/0001.md"))
	if first != second {
		t.Errorf("ContentHash is not stable across runs: %s vs %s", first, second)
	}
}

// TestForceRewriteReAssertsEveryNode is the escape hatch: when the version mix is
// missed, or a destination was edited out of band in a way no hash can see, the
// operator can rewrite the mirror without reaching into it by hand and deleting
// state files.
func TestForceRewriteReAssertsEveryNode(t *testing.T) {
	b := loadBundle(t, workedExample())
	all := []string{"index.md", "docs/adr/index.md", "docs/adr/0001.md", "docs/adr/0002.md", "CONTEXT.md"}
	cs := seed{
		unchanged: all,
		anchors:   map[publish.AnchorName]any{"glossary/root-kek": nil},
	}.build(t, b)

	// Without it, this is the reported symptom: nothing to do.
	if g := gen(t, b, cs); len(g.Ops) != 0 {
		t.Fatalf("an unchanged bundle emitted %d ops; the fixture is not steady-state", len(g.Ops))
	}

	g, err := Generate(context.Background(), b, cs, WithForceRewrite())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, rel := range all {
		node := nodeRef(rel)
		if opFor(g, node, SetContent) == nil {
			t.Errorf("%s was not re-asserted: no SetContent", rel)
		}
		if opFor(g, node, SetProperties) == nil {
			t.Errorf("%s was not re-asserted: no SetProperties", rel)
		}
	}
	// A forced rewrite re-asserts what exists; it does not re-create it.
	for _, op := range g.Ops {
		if op.Kind == CreateNode {
			t.Errorf("a forced rewrite emitted a CreateNode for %s, which already exists", op.Node)
		}
	}
}

// A forced rewrite is about EXISTING nodes; a node the destination has never seen
// is still a create, and a vanished one is still a delete.
func TestForceRewriteLeavesCreatesAndDeletesAlone(t *testing.T) {
	files := map[string]string{
		"okf.toml": "",
		"index.md": "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"new.md":   "---\ntype: c\n---\nBrand new.\n",
	}
	b := loadBundle(t, files)
	// index.md is published and unchanged; gone.md is in the scan with no source.
	cs := publish.NewCurrentStateWithProps(
		map[publish.SymbolicID]publish.BackendID{
			nodeRef("index.md"): "be-index",
			nodeRef("gone.md"):  "be-gone",
		},
		map[publish.SymbolicID]publish.Hash{nodeRef("index.md"): ContentHash(docByRel(t, b, "index.md"))},
		map[publish.SymbolicID]publish.Hash{nodeRef("index.md"): PropertyHash(docByRel(t, b, "index.md"))},
		nil,
	)

	g, err := Generate(context.Background(), b, cs, WithForceRewrite())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if opFor(g, nodeRef("new.md"), CreateNode) == nil {
		t.Error("a new page was not created under a forced rewrite")
	}
	if opFor(g, nodeRef("gone.md"), DeleteNode) == nil {
		t.Error("a vanished page was not deleted under a forced rewrite")
	}
	if opFor(g, nodeRef("index.md"), SetContent) == nil {
		t.Error("an unchanged page was not re-asserted under a forced rewrite")
	}
}

// TestContentDigestIsSeededWithTheRendererVersion pins the seam that replaced the
// remember-to-fold-it-in comment: a digest is ALREADY carrying the renderer
// version before a caller writes a byte, so a hasher cannot omit it by forgetting.
func TestContentDigestIsSeededWithTheRendererVersion(t *testing.T) {
	empty := sha256.Sum256(nil)
	if got := NewContentDigest().Sum(); string(got) == hex.EncodeToString(empty[:]) {
		t.Fatal("a fresh digest must already carry the renderer version, not be an empty hash")
	}
	if NewContentDigestAt(RendererVersion).Sum() != NewContentDigest().Sum() {
		t.Error("NewContentDigest must seed at the current RendererVersion")
	}
	if NewContentDigestAt(RendererVersion+1).Sum() == NewContentDigest().Sum() {
		t.Error("a version bump must move the seed, and so every hash built on it")
	}
}

// TestContentDigestSeparatesVersionFromContent is why the version is a NUL-separated
// PREFIX: no content can be crafted that makes one version's digest collide with
// another's.
func TestContentDigestSeparatesVersionFromContent(t *testing.T) {
	a := NewContentDigestAt(1)
	a.Writef("%s", "2\x00payload")
	b := NewContentDigestAt(12)
	b.Writef("%s", "payload")
	if a.Sum() == b.Sum() {
		t.Error("content must not be able to forge another renderer version's hash")
	}
}
