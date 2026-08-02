// External test package: it imports graph/optimize/transport (which build ON the
// fs backend's role interfaces) to drive the WHOLE pipeline against the filesystem
// backend, offline, and assert the exported tree. That is the "seam didn't leak
// Notion specifics" regression check (sigma/ideas#172, decision 7): the exact same
// Generation → Optimization → Transport code path that publishes to Notion also
// exports to disk, with no special-casing anywhere in the pipeline.
package fs_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
	fsbackend "github.com/sigma/okf-tools/internal/publish/backend/fs"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/optimize"
	"github.com/sigma/okf-tools/internal/publish/transport"
)

// loadBundle materializes an in-memory file set as a real okf bundle on disk and
// loads it through the production discover/load path, so the export drives Stage 1
// against genuine parsed input.
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

// exportBundle is a small but representative bundle exercising every edge cause the
// pipeline must sequence — nested indexes (parent-before-child), a cross-document
// link (content-refs-node), and a glossary hosting a cited anchor
// (content-refs-anchor) — WITHOUT a mutual new-page link, which an unbounded,
// non-fusing bin would (like Notion at its real 100-block cap) turn into a genuine
// transaction cycle. The link a→b is one-directional.
func exportBundle() map[string]string {
	return map[string]string{
		"okf.toml":      "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md":      "---\nokf_version: \"0.1\"\n---\n# Root\n",
		"docs/index.md": "# Docs\n",
		"docs/a.md":     "---\ntype: adr\ntitle: A\n---\nSee [B](b.md) and the [root KEK](../CONTEXT.md#root-kek).\n",
		"docs/b.md":     "---\ntype: adr\ntitle: B\n---\nJust B, standalone.\n",
		"CONTEXT.md":    "# Glossary\n\n**Root KEK**: the root key-encryption key.\n",
	}
}

// publishToDisk runs the real three-stage pipeline against a filesystem backend
// rooted at out and returns the transport result. It mirrors transport's own
// end-to-end test wiring, proving the pipeline is unchanged for this backend.
func publishToDisk(t *testing.T, b *bundle.Bundle, out string) *transport.Result {
	t.Helper()
	be := fsbackend.New(fsbackend.WithRoot(out))

	scan, err := be.Scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	g, err := graph.Generate(context.Background(), b, scan)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	dag := optimize.Optimize(g, be, be)
	res, err := transport.New(be, transport.WithInterval(0)).Run(context.Background(), dag, scan)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	return res
}

// TestExportTree publishes the bundle to a filesystem backend and asserts the
// exported tree file-by-file — the seam-didn't-leak regression check.
func TestExportTree(t *testing.T) {
	b := loadBundle(t, exportBundle())
	out := t.TempDir()

	res := publishToDisk(t, b, out)

	// Every source node was created and resolved to its on-disk path.
	wantNodes := []string{"index.md", "docs/index.md", "docs/a.md", "docs/b.md", "CONTEXT.md"}
	for _, rel := range wantNodes {
		id := publish.SymbolicID("node:" + rel)
		got, ok := res.Nodes[id]
		if !ok {
			t.Errorf("node %s never resolved", id)
			continue
		}
		if string(got) != rel {
			t.Errorf("node %s resolved to %q, want on-disk id %q", id, got, rel)
		}
	}

	// Each node directory carries its existence marker, its properties, and at
	// least one content section — one file per unit, nothing fused.
	for _, rel := range wantNodes {
		dir := filepath.Join(out, filepath.FromSlash(rel))
		mustExist(t, filepath.Join(dir, "node.json"))
		mustExist(t, filepath.Join(dir, "props.json"))
		mustExist(t, filepath.Join(dir, "0000.md"))
	}

	// The cross-document link and the glossary anchor both resolved to on-disk
	// targets inside a.md's rendered section.
	aBody := readFile(t, filepath.Join(out, "docs", "a.md", "0000.md"))
	if !strings.Contains(aBody, "(docs/b.md)") {
		t.Errorf("a.md content did not resolve the node link to docs/b.md:\n%s", aBody)
	}
	if !strings.Contains(aBody, "(CONTEXT.md#glossary/root-kek)") {
		t.Errorf("a.md content did not resolve the anchor link:\n%s", aBody)
	}

	// The glossary hosted the anchor: it resolved in the table and was written to
	// the host's anchors.json.
	anchor := publish.AnchorName("glossary/root-kek")
	if got, ok := res.Anchors[anchor]; !ok || string(got) != "CONTEXT.md#glossary/root-kek" {
		t.Errorf("anchor %s resolved to %q,%v, want CONTEXT.md#glossary/root-kek", anchor, got, ok)
	}
	anchors := readFile(t, filepath.Join(out, "CONTEXT.md", "anchors.json"))
	if !strings.Contains(anchors, "glossary/root-kek") {
		t.Errorf("CONTEXT.md/anchors.json missing the hosted anchor:\n%s", anchors)
	}

	// The heading of the root index rendered with a Markdown prefix (not a Notion
	// block) — the section format is the filesystem backend's, not Notion's.
	rootBody := readFile(t, filepath.Join(out, "index.md", "0000.md"))
	if !strings.HasPrefix(rootBody, "# ") {
		t.Errorf("root index heading did not render as Markdown: %q", rootBody)
	}

	// Only the expected node directories exist — no stray writes.
	assertNodeDirs(t, out, wantNodes)
}

// TestExportDeterministic re-exports the same bundle to a second tree and asserts
// the two trees are byte-identical — reproducible dry-runs.
func TestExportDeterministic(t *testing.T) {
	b := loadBundle(t, exportBundle())
	out1, out2 := t.TempDir(), t.TempDir()
	publishToDisk(t, b, out1)
	publishToDisk(t, b, out2)

	tree1 := snapshot(t, out1)
	tree2 := snapshot(t, out2)
	if len(tree1) != len(tree2) {
		t.Fatalf("tree file counts differ: %d vs %d", len(tree1), len(tree2))
	}
	for rel, content := range tree1 {
		if tree2[rel] != content {
			t.Errorf("file %s differs between runs:\n--- run1 ---\n%s\n--- run2 ---\n%s", rel, content, tree2[rel])
		}
	}
}

// TestScanRoundTrip proves the disk-read Scanner recovers what Execute wrote: a
// scan of a populated export tree reports every node id and the hosted anchor.
func TestScanRoundTrip(t *testing.T) {
	b := loadBundle(t, exportBundle())
	out := t.TempDir()
	publishToDisk(t, b, out)

	be := fsbackend.New(fsbackend.WithRoot(out))
	cs, err := be.Scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, rel := range []string{"index.md", "docs/index.md", "docs/a.md", "docs/b.md", "CONTEXT.md"} {
		id := publish.SymbolicID("node:" + rel)
		if got, ok := cs.NodeID(id); !ok || string(got) != rel {
			t.Errorf("scan NodeID(%s) = %q,%v, want %q", id, got, ok, rel)
		}
	}
	if got, ok := cs.AnchorID("glossary/root-kek"); !ok || string(got) != "CONTEXT.md#glossary/root-kek" {
		t.Errorf("scan AnchorID(glossary/root-kek) = %q,%v, want CONTEXT.md#glossary/root-kek", got, ok)
	}
}

// TestScanEmptyRoot proves a missing output directory scans as an empty snapshot —
// the fresh-export case — rather than erroring.
func TestScanEmptyRoot(t *testing.T) {
	be := fsbackend.New(fsbackend.WithRoot(filepath.Join(t.TempDir(), "does-not-exist")))
	cs, err := be.Scan(context.Background())
	if err != nil {
		t.Fatalf("scan of missing root: %v", err)
	}
	for range cs.Nodes() {
		t.Fatal("missing root should scan to zero nodes")
	}
}

// --- helpers ----------------------------------------------------------------

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file missing: %s (%v)", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// assertNodeDirs walks the export root and asserts exactly the expected set of
// node directories (those holding a node.json) exists.
func assertNodeDirs(t *testing.T, root string, want []string) {
	t.Helper()
	var got []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || info.Name() != "node.json" {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(got)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if strings.Join(got, ",") != strings.Join(wantSorted, ",") {
		t.Errorf("node dirs = %v, want %v", got, wantSorted)
	}
}

// snapshot reads every regular file under root into a rel-path → content map for
// byte-for-byte tree comparison.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot walk: %v", err)
	}
	return out
}
