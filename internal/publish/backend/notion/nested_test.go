package notion

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/optimize"
	"github.com/sigma/okf-tools/internal/publish/transport"
)

// loadNestedBundle materializes a bundle TWO index levels deep: a root index, a
// cluster index below it, and a leaf below that. The leaf's parent is therefore
// itself a subpage, which is the shape #141 is about.
func loadNestedBundle(t *testing.T) *bundle.Bundle {
	return loadBundleFiles(t, map[string]string{
		"okf.toml":          "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md":          "---\nokf_version: \"0.1\"\n---\n# Root\n",
		"CONTEXT.md":        "# Glossary\n\n**Root KEK**: the root key-encryption key.\n",
		"docs/adr/index.md": "# ADRs\n",
		"docs/adr/a.md":     "---\ntype: adr\ntitle: A\n---\nBody of A.\n",
		"docs/adr/b.md":     "---\ntype: adr\ntitle: B\n---\nBody of B.\n",
	})
}

func loadBundleFiles(t *testing.T, files map[string]string) *bundle.Bundle {
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

// runPublish drives Scan → Generate → Optimize → Transport, the pipeline's own
// wiring, and reports the ops planned.
func runPublish(t *testing.T, be *Backend, b *bundle.Bundle, mode backend.ScanMode) ([]*graph.Op, error) {
	t.Helper()
	ctx := context.Background()
	scan, err := be.Scan(ctx, mode)
	if err != nil {
		return nil, err
	}
	g, err := graph.Generate(ctx, b, scan, graph.WithHasher(be.RecomputeContentHasher(nil)))
	if err != nil {
		return nil, err
	}
	_, err = transport.New(be).Run(ctx, optimize.Optimize(g, be, be), scan)
	return g.Ops, err
}

// A bundle two index levels deep publishes. Its leaf's record cannot go to its
// immediate parent, which is a child_page with no data-source columns.
func TestNestedClusterPublishes(t *testing.T) {
	b := loadNestedBundle(t)
	be := newServer(t, newFakeNotion())

	if _, err := runPublish(t, be, b, backend.ScanStored); err != nil {
		t.Fatalf("nested bundle failed to publish: %v", err)
	}
}

// The leaf's record lands in the nearest ancestor ROW's subtree map, keyed by its
// bundle-relative path, beside the cluster index's own entry.
func TestNestedRecordsClimbToTheOwningRow(t *testing.T) {
	b := loadNestedBundle(t)
	f := newFakeNotion()
	be := newServer(t, f)

	if _, err := runPublish(t, be, b, backend.ScanStored); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// The only row is the root index; everything else is a page under it.
	scan, err := be.Scan(context.Background(), backend.ScanStored)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, rel := range []string{"CONTEXT.md", "docs/adr/index.md", "docs/adr/a.md", "docs/adr/b.md"} {
		if _, ok := scan.NodeID(publish.NodeRef(rel)); !ok {
			t.Errorf("%s is not readable from the mirror's own record", rel)
		}
	}

	// No column property was ever written to a child_page — the invariant that broke.
	for _, req := range f.reqs {
		if req.Method != http.MethodPatch || req.Body["properties"] == nil {
			continue
		}
		props, _ := req.Body["properties"].(map[string]any)
		id := req.Path[len("/pages/"):]
		f.mu.Lock()
		child := f.childPages[id]
		f.mu.Unlock()
		if !child {
			continue
		}
		for name := range props {
			if name != "title" {
				t.Errorf("wrote column %q to child_page %s", name, id)
			}
		}
	}
}

// A re-run of the nested bundle is a near-noop: every node hash-skips.
func TestNestedClusterRerunIsANoop(t *testing.T) {
	b := loadNestedBundle(t)
	be := newServer(t, newFakeNotion())

	if _, err := runPublish(t, be, b, backend.ScanStored); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	ops, err := runPublish(t, be, b, backend.ScanStored)
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if len(ops) != 0 {
		var kinds []string
		for _, op := range ops {
			kinds = append(kinds, op.Kind.String()+" "+string(op.Node))
		}
		t.Errorf("re-run planned %d op(s), want none: %v", len(ops), kinds)
	}
}

// A --recompute scan walks the grandchild's live blocks rather than falling back to
// its stored hash — otherwise the mode reports coverage it does not have.
func TestRecomputeWalksNestedGrandchildren(t *testing.T) {
	b := loadNestedBundle(t)
	f := newFakeNotion()
	be := newServer(t, f)

	if _, err := runPublish(t, be, b, backend.ScanStored); err != nil {
		t.Fatalf("publish: %v", err)
	}

	scan, err := be.Scan(context.Background(), backend.ScanRecompute)
	if err != nil {
		t.Fatalf("recompute scan: %v", err)
	}
	leaf := publish.NodeRef("docs/adr/a.md")
	id, ok := scan.NodeID(leaf)
	if !ok {
		t.Fatalf("recompute lost the grandchild %s", leaf)
	}
	if n := f.countPath(http.MethodGet, "/blocks/"+string(id)+"/children"); n == 0 {
		t.Errorf("recompute never walked the grandchild's blocks (page %s)", id)
	}

	ops, err := runPublish(t, be, b, backend.ScanRecompute)
	if err != nil {
		t.Fatalf("recompute publish: %v", err)
	}
	if len(ops) != 0 {
		t.Errorf("recompute on a converged nested mirror planned %d op(s), want none", len(ops))
	}
}

// storedMapOf reads a row's recorded subtree map back the way a scan does.
func storedMapOf(t *testing.T, f *fakeNotion, rowID string) map[string]subtreeEntry {
	t.Helper()
	f.mu.Lock()
	var raw any
	for _, row := range f.rows {
		if row["id"] != rowID {
			continue
		}
		props, _ := row["properties"].(map[string]any)
		raw = props["hashes"]
	}
	f.mu.Unlock()

	encoded, err := json.Marshal(map[string]any{"hashes": raw})
	if err != nil {
		t.Fatalf("re-encode row props: %v", err)
	}
	var served struct {
		Hashes property `json:"hashes"`
	}
	if err := json.Unmarshal(encoded, &served); err != nil {
		t.Fatalf("decode row props: %v", err)
	}
	m, err := storedSubtree(plainText(served.Hashes), rowID)
	if err != nil {
		t.Fatalf("decode subtree map: %v", err)
	}
	return m
}

// theOnlyRow returns the single data-source row's id, failing if the mirror holds
// more than one (these fixtures put everything under a root index).
func theOnlyRow(t *testing.T, f *fakeNotion) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rows) != 1 {
		t.Fatalf("want exactly one row, got %d", len(f.rows))
	}
	return f.rows[0]["id"].(string)
}

// The grandchild's entry sits in the owning row's map keyed by its bundle-relative
// path, BESIDE the child's — one flat map per row, whatever the depth.
func TestOwningRowMapHoldsEveryDescendantByPath(t *testing.T) {
	b := loadNestedBundle(t)
	f := newFakeNotion()
	be := newServer(t, f)

	if _, err := runPublish(t, be, b, backend.ScanStored); err != nil {
		t.Fatalf("publish: %v", err)
	}

	stored := storedMapOf(t, f, theOnlyRow(t, f))
	for _, subpath := range []string{"CONTEXT.md", "docs/adr/index.md", "docs/adr/a.md", "docs/adr/b.md"} {
		e, ok := stored[subpath]
		if !ok {
			t.Errorf("owning row's map is missing %s; it holds %v", subpath, stored)
			continue
		}
		if e.ID == "" || e.Hash == "" {
			t.Errorf("entry for %s = %+v, want a recorded id and hash", subpath, e)
		}
	}
}

// A partial run records correctly: when only a grandchild changes, its owning row is
// NOT touched this run, so the owner resolves from the scan seed rather than from
// anything the run minted. This is why the owning row is stamped by Generation — the
// backend could not climb a chain whose members are absent from the provenance.
func TestPartialRunRecordsAgainstAnUntouchedOwner(t *testing.T) {
	files := map[string]string{
		"okf.toml":          "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md":          "---\nokf_version: \"0.1\"\n---\n# Root\n",
		"CONTEXT.md":        "# Glossary\n\n**Root KEK**: the root key-encryption key.\n",
		"docs/adr/index.md": "# ADRs\n",
		"docs/adr/a.md":     "---\ntype: adr\ntitle: A\n---\nBody of A.\n",
		"docs/adr/b.md":     "---\ntype: adr\ntitle: B\n---\nBody of B.\n",
	}
	f := newFakeNotion()
	be := newServer(t, f)
	if _, err := runPublish(t, be, loadBundleFiles(t, files), backend.ScanStored); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	rowID := theOnlyRow(t, f)
	before := storedMapOf(t, f, rowID)["docs/adr/a.md"]

	// Edit the grandchild alone. Its owning row (the root index) and its parent (the
	// cluster index) both hash-skip, so neither is in this run's provenance.
	files["docs/adr/a.md"] = "---\ntype: adr\ntitle: A\n---\nBody of A, revised.\n"
	edited := loadBundleFiles(t, files)
	ops, err := runPublish(t, be, edited, backend.ScanStored)
	if err != nil {
		t.Fatalf("partial publish: %v", err)
	}
	for _, op := range ops {
		if op.Node != publish.NodeRef("docs/adr/a.md") {
			t.Errorf("partial run touched %s (%s); only the edited grandchild should move", op.Node, op.Kind)
		}
	}

	after := storedMapOf(t, f, rowID)["docs/adr/a.md"]
	if after.Hash == "" || after.Hash == before.Hash {
		t.Errorf("grandchild's recorded hash = %q (was %q); the partial run did not record it", after.Hash, before.Hash)
	}
	// Everything else in the map survived the merge.
	final := storedMapOf(t, f, rowID)
	for _, subpath := range []string{"CONTEXT.md", "docs/adr/index.md", "docs/adr/b.md"} {
		if _, ok := final[subpath]; !ok {
			t.Errorf("partial run dropped %s from the owning row's map", subpath)
		}
	}
	// And the mirror is converged again.
	next, err := runPublish(t, be, edited, backend.ScanStored)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if len(next) != 0 {
		t.Errorf("re-run after the partial publish planned %d op(s), want none", len(next))
	}
}

// writeSubtreeMap replaces a row's recorded subtree map, so a test can stage the
// state an older version of the writer would have left.
func writeSubtreeMap(t *testing.T, f *fakeNotion, rowID string, m map[string]subtreeEntry) {
	t.Helper()
	enc, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode subtree map: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if row["id"] != rowID {
			continue
		}
		props, _ := row["properties"].(map[string]any)
		props["hashes"] = richProp(string(enc))
	}
	f.pageProps[rowID] = map[string]any{"hashes": richProp(string(enc))}
}
