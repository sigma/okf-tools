package transport

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend/fake"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/optimize"
)

// loadBundle materializes an in-memory file set as a real okf bundle on disk and
// loads it through the production discover/load path, so the end-to-end test
// drives Stage 1 against genuine parsed input rather than a hand-built op-DAG.
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

// e2eBundle is a small but representative bundle: nested indexes (parent-before-
// child edges), a cross-document link (a → b, content-refs-node), and a glossary
// that hosts an anchor a page cites (content-refs-anchor). Every mechanism the
// transport must sequence through the resolution table therefore appears.
func e2eBundle() map[string]string {
	return map[string]string{
		"okf.toml":          "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md":          "---\nokf_version: \"0.1\"\n---\n# Root\n",
		"docs/adr/index.md": "# ADRs\n",
		"docs/adr/a.md":     "---\ntype: adr\ntitle: A\n---\nSee [B](b.md) and the [root KEK](../../CONTEXT.md#root-kek).\n",
		"docs/adr/b.md":     "---\ntype: adr\ntitle: B\n---\nSee [A](a.md).\n",
		"CONTEXT.md":        "# Glossary\n\n**Root KEK**: the root key-encryption key.\n",
	}
}

// pipeline runs the whole publish — Generation, Optimization, Transport — against
// a fresh fake backend and returns the transport Result plus the fake so a caller
// can inspect the recorded transactions.
func pipeline(t *testing.T, files map[string]string) (*Result, *optimize.TxnDAG, *fake.Backend) {
	t.Helper()
	b := loadBundle(t, files)
	// maxCount=2 dials packing pressure so each node's create+props seal into one
	// transaction and its content overflows into another. That separation is what
	// keeps a mutual link (a ↔ b, both new) acyclic: the transport can execute both
	// creates, then both contents — the create-before-content sequencing the whole
	// pipeline exists to guarantee. (Unbounded bins would fuse create+content per
	// node and a mutual link between two new pages would be a genuine cycle — an
	// optimizer/backend fusion concern, out of scope for transport.)
	be := fake.New(fake.WithMaxCount(2)) // empty scan by default

	scan, err := be.Scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	g, err := graph.Generate(context.Background(), b, scan)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	dag := optimize.Optimize(g, be, be)

	res, err := New(be, WithInterval(0)).Run(context.Background(), dag, scan)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	return res, dag, be
}

// TestEndToEndPublish is the headline milestone: a full bundle publishes into the
// in-memory fake store, offline, wiring real Stage-1 generation + real Stage-2
// optimization + the transport. It asserts the fake recorded one transaction per
// PackedTxn and that every source node resolved in the final table — proof the
// readiness gate sequenced every cross-link and anchor edge to completion.
func TestEndToEndPublish(t *testing.T) {
	files := e2eBundle()
	res, dag, be := pipeline(t, files)

	if got, want := len(be.Executed()), len(dag.Txns); got != want {
		t.Errorf("executed %d transactions, want one per PackedTxn (%d)", got, want)
	}

	// Every source page (all new against an empty scan) must resolve to a minted
	// backend id.
	b := loadBundle(t, files)
	for _, d := range b.Docs {
		id := publish.SymbolicID("node:" + d.Rel)
		if _, ok := res.Nodes[id]; !ok {
			t.Errorf("node %s never resolved in the final table", id)
		}
	}

	// The glossary anchor the page cites must have been hosted and resolved.
	if _, ok := res.Anchors[publish.AnchorName("glossary/root-kek")]; !ok {
		t.Errorf("anchor glossary/root-kek never resolved, anchors=%v", res.Anchors)
	}
}

// TestEndToEndDeterministic runs the whole pipeline twice against two fresh fake
// backends and asserts byte-identical resolution tables. Because minting is
// sequence-based and the drain order is deterministic, identical inputs yield
// identical outputs — the offline-and-deterministic property the milestone needs.
func TestEndToEndDeterministic(t *testing.T) {
	files := e2eBundle()
	first, _, _ := pipeline(t, files)
	second, _, _ := pipeline(t, files)

	if len(first.Nodes) != len(second.Nodes) {
		t.Fatalf("node count differs: %d vs %d", len(first.Nodes), len(second.Nodes))
	}
	for id, b := range first.Nodes {
		if second.Nodes[id] != b {
			t.Errorf("node %s: run1=%s run2=%s (nondeterministic mint)", id, b, second.Nodes[id])
		}
	}
	if len(first.Anchors) != len(second.Anchors) {
		t.Fatalf("anchor count differs: %d vs %d", len(first.Anchors), len(second.Anchors))
	}
	for name, b := range first.Anchors {
		if second.Anchors[name] != b {
			t.Errorf("anchor %s: run1=%s run2=%s", name, b, second.Anchors[name])
		}
	}
}
