// External test package: it drives the WHOLE pipeline (Generation → Optimization
// → Transport) against the prototype Google Docs backend, offline, exactly as the
// fs backend's suite does. The point is not to assert a rendering — it is to find
// out whether the seam carries a document-shaped destination at all.
package gdocs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish/backend/gdocs"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/optimize"
	"github.com/sigma/okf-tools/internal/publish/transport"
)

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

// protoBundle exercises the three edge causes the pipeline must sequence: a
// parent-before-child index, a cross-page link, and a glossary anchor citation.
func protoBundle() map[string]string {
	return map[string]string{
		"okf.toml":   "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md":   "---\nokf_version: \"0.1\"\ntitle: Index\ntype: index\n---\n\n# Index\n\n- [Alpha](/alpha.md)\n- [Beta](/beta.md)\n",
		"CONTEXT.md": "---\nokf_version: \"0.1\"\ntitle: Context\ntype: context\n---\n\n# Glossary\n\n**Widget** — a thing.\n",
		"alpha.md":   "---\nokf_version: \"0.1\"\ntitle: Alpha\ntype: concept\n---\n\n# Alpha\n\nAlpha links to [Beta](/beta.md) and cites a **Widget**.\n",
		"beta.md":    "---\nokf_version: \"0.1\"\ntitle: Beta\ntype: concept\n---\n\n# Beta\n\nBeta stands alone.\n",
	}
}

func run(t *testing.T, be *gdocs.Backend, b *bundle.Bundle) int {
	t.Helper()
	ctx := context.Background()
	scan, err := be.Scan(ctx, 0)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	g, err := graph.Generate(ctx, b, scan)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	dag := optimize.Optimize(g, be, be)
	if _, err := transport.New(be).Run(ctx, dag, scan); err != nil {
		t.Fatalf("transport: %v", err)
	}
	return len(dag.Txns)
}

// TestPipelineDrivesGoogleDocsShape is the load-bearing question: does the
// existing pipeline drive a document-of-tabs backend end to end?
func TestPipelineDrivesGoogleDocsShape(t *testing.T) {
	b := loadBundle(t, protoBundle())
	be := gdocs.New()

	txns := run(t, be, b)
	batch, gets, tabs := be.Stats()
	t.Logf("first run: %d transactions, %d batchUpdates, %d gets, %d tabs", txns, batch, gets, tabs)
	t.Logf("document:\n%s", be.Dump())
	if len(be.Overflow()) > 0 {
		t.Logf("write-back overflow: %v", be.Overflow())
	}

	if tabs != 4 {
		t.Errorf("want a tab per page (4), got %d", tabs)
	}
	if !strings.Contains(be.Dump(), "<link ") {
		t.Error("no cross-reference resolved to a tab link")
	}
	// #155: tabs are flat — no tab carries a parent — yet the index tab still shows
	// the hierarchy as links to its children.
	if strings.Contains(be.Dump(), `parent="t.`) {
		t.Error("flat tabs expected, but a tab carries a parentTabId")
	}
}

// TestRerunIsNearNoop checks the property the whole change-detection design rests
// on: an unchanged second run should touch nothing.
func TestRerunIsNearNoop(t *testing.T) {
	b := loadBundle(t, protoBundle())
	be := gdocs.New()
	run(t, be, b)
	before, _, _ := be.Stats()
	t.Logf("appProperties after run 1: %v", be.Props())
	t.Logf("overflow after run 1: %v", be.Overflow())

	second := run(t, be, b)
	after, _, tabs := be.Stats()
	t.Logf("second run: %d transactions, batchUpdates %d -> %d, %d tabs", second, before, after, tabs)
	if second != 0 {
		t.Errorf("unchanged re-run should emit no transactions, got %d", second)
	}
	if tabs != 4 {
		t.Errorf("re-run duplicated tabs: %d", tabs)
	}
}
