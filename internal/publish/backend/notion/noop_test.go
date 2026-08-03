package notion

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/optimize"
	"github.com/sigma/okf-tools/internal/publish/transport"
)

// loadNoopBundle materializes a small bundle on disk and loads it through the
// production discover/load path, so the near-noop proof drives real Stage-1
// generation against genuine parsed input.
func loadNoopBundle(t *testing.T) *bundle.Bundle {
	t.Helper()
	files := map[string]string{
		"okf.toml":          "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md":          "---\nokf_version: \"0.1\"\n---\n# Root\n",
		"docs/adr/index.md": "# ADRs\n",
		"docs/adr/a.md":     "---\ntype: adr\ntitle: A\n---\nSee [B](b.md) and the [root KEK](../../CONTEXT.md#root-kek).\n",
		"docs/adr/b.md":     "---\ntype: adr\ntitle: B\n---\nSee [A](a.md).\n",
		"CONTEXT.md":        "# Glossary\n\n**Root KEK**: the root key-encryption key.\n",
	}
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

// TestNotionNearNoopRerun proves the near-noop re-run property against the faked
// Notion HTTP surface: when the ScanStored query reports every source page already
// present with its expected content hash, Generation hash-skips them all, the
// transaction-DAG is empty, and the transport issues ZERO write requests to Notion
// (no POST /pages, no PATCH). The only Notion round-trip the whole publish spends is
// the single steady-state data-source query — exactly the "unchanged run makes
// near-zero API calls" acceptance criterion, demonstrated end to end and offline.
func TestNotionNearNoopRerun(t *testing.T) {
	b := loadNoopBundle(t)

	// Seed the fake data source with a row per source page whose `hash` column is the
	// same content hash Generation computes — the write-back a real steady state would
	// have left behind (#44).
	f := newFakeNotion()
	be := newServer(t, f)
	for _, d := range b.Docs {
		// Seed each row's `hash` column with the compound content.prop value the
		// steady state would have left — the aligned source content hash the wired
		// hasher now computes, paired with the property hash (#110 phase 2).
		f.rows = append(f.rows, row("page-"+d.Rel, map[string]any{
			"path": richProp(d.Rel),
			"hash": richProp(encodeHashPair(be.sourceContentHash(d, nil), graph.PropertyHash(d))),
		}))
	}

	ctx := context.Background()
	scan, err := be.Scan(ctx, backend.ScanStored)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	// Wire the same source-side hasher the pipeline wires for this backend, so change
	// detection compares the aligned content hash against the seeded one (#110).
	g, err := graph.Generate(ctx, b, scan, graph.WithHasher(be.RecomputeContentHasher(nil)))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(g.Ops) != 0 {
		t.Fatalf("unchanged bundle should emit no ops, got %d: %+v", len(g.Ops), g.Ops)
	}

	dag := optimize.Optimize(g, be, be)
	if len(dag.Txns) != 0 {
		t.Fatalf("unchanged bundle should optimize to no transactions, got %d", len(dag.Txns))
	}

	if _, err := transport.New(be, transport.WithInterval(0)).Run(ctx, dag, scan); err != nil {
		t.Fatalf("transport: %v", err)
	}

	// Zero writes hit Notion — the whole point of the steady-state publish. The only
	// requests the run may make are the data-source query the scan spends; anything
	// else (POST /pages, PATCH /pages/{id}, PATCH /blocks/{id}/children) is a write.
	for _, r := range f.reqs {
		if r.Method != "POST" || !strings.HasSuffix(r.Path, "/query") {
			t.Errorf("near-noop re-run issued a non-query request: %s %s", r.Method, r.Path)
		}
	}
	if f.countPath("POST", "/pages") != 0 {
		t.Errorf("near-noop re-run created pages, want 0")
	}
}
