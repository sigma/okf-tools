package notion

import (
	"context"
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

// loadAnchorBundle materializes a flat bundle whose pages are all TOP-LEVEL rows:
// a glossary host and two pages citing its anchors, with no covering index above
// them (a root index would make every page a child_page subpage instead, which
// exercises the subtree write-back rather than the anchor seeding this file is
// about). Loaded through the production discover/load path, so the tests drive real
// parsed input.
func loadAnchorBundle(t *testing.T) *bundle.Bundle {
	t.Helper()
	files := map[string]string{
		"okf.toml":   "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"CONTEXT.md": "# Glossary\n\n**Root KEK**: the root key-encryption key.\n\n**Envoy**: the agent that carries a payload.\n",
		"a.md":       "---\ntype: adr\ntitle: A\n---\nThe [root KEK](CONTEXT.md#root-kek) protects everything.\n",
		"b.md":       "---\ntype: adr\ntitle: B\n---\nAn [envoy](CONTEXT.md#envoy) carries it, guarded by the [root KEK](CONTEXT.md#root-kek).\n",
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

// publishThrough drives one full publish of b against be, exactly as the pipeline
// wires it, and reports the ops the run planned. Every test in this file works from
// state a REAL publish produced rather than hand-seeded blocks: a scan keyed on a
// block shape the executor cannot write passes offline and fails on every live
// mirror, which is precisely how #138 survived.
func publishThrough(t *testing.T, be *Backend, b *bundle.Bundle, mode backend.ScanMode) []*graph.Op {
	t.Helper()
	ctx := context.Background()
	scan, err := be.Scan(ctx, mode)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	g, err := graph.Generate(ctx, b, scan, graph.WithHasher(be.RecomputeContentHasher(nil)))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := transport.New(be).Run(ctx, optimize.Optimize(g, be, be), scan); err != nil {
		t.Fatalf("transport: %v", err)
	}
	return g.Ops
}

// anchorsCited collects every anchor the bundle's pages link to, which is what a
// run must be able to resolve before it can proceed.
func anchorsCited(t *testing.T, be *Backend, b *bundle.Bundle) []publish.AnchorName {
	t.Helper()
	g, err := graph.Generate(context.Background(), b, publish.NewCurrentState(nil, nil, nil),
		graph.WithHasher(be.RecomputeContentHasher(nil)))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	seen := map[publish.AnchorName]bool{}
	var out []publish.AnchorName
	for _, op := range g.Ops {
		for _, ref := range op.Refs {
			if name, ok := ref.AnchorName(); ok && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("the fixture bundle cites no anchors; it cannot exercise anchor seeding")
	}
	return out
}

// The round trip #138 is about: publish a bundle, then recompute-scan the state that
// publish produced. Every anchor the corpus cites must resolve. Against a scan that
// rebuilds the map from bold-lead blocks this fails outright, because the executor
// writes no annotations for any block to lead with.
func TestRecomputeSeedsAnchorsAfterARealPublish(t *testing.T) {
	b := loadAnchorBundle(t)
	f := newFakeNotion()
	be := newServer(t, f)

	publishThrough(t, be, b, backend.ScanStored)

	scan, err := be.Scan(context.Background(), backend.ScanRecompute)
	if err != nil {
		t.Fatalf("recompute scan: %v", err)
	}
	for _, name := range anchorsCited(t, be, b) {
		if id, ok := scan.AnchorID(name); !ok || id == "" {
			t.Errorf("recompute seed missing anchor %s (id=%q)", name, id)
		}
	}
}

// A converged mirror recompute-scans to a no-op, the same near-noop property the
// steady-state scan has. A run that re-asserts everything is not detecting drift,
// it is causing it.
func TestRecomputeOnAConvergedMirrorIsANoop(t *testing.T) {
	b := loadAnchorBundle(t)
	f := newFakeNotion()
	be := newServer(t, f)

	publishThrough(t, be, b, backend.ScanStored)

	writesBefore := f.countPath("POST", "/pages") + f.countPath("PATCH", "/pages")
	ops := publishThrough(t, be, b, backend.ScanRecompute)
	if len(ops) != 0 {
		var kinds []string
		for _, op := range ops {
			kinds = append(kinds, op.Kind.String()+" "+string(op.Node))
		}
		t.Errorf("recompute on a converged mirror planned %d op(s), want none: %v", len(ops), kinds)
	}
	if got := f.countPath("POST", "/pages") + f.countPath("PATCH", "/pages"); got != writesBefore {
		t.Errorf("recompute no-op wrote to the mirror: %d writes before, %d after", writesBefore, got)
	}
}

// A recorded anchor whose block is no longer a live child is DANGLING — the state an
// interrupted run leaves behind. The host page must be re-asserted even though its
// content still matches source, because re-asserting is what re-mints the map.
func TestRecomputeReassertsAHostWithDanglingAnchors(t *testing.T) {
	b := loadAnchorBundle(t)
	f := newFakeNotion()
	be := newServer(t, f)

	publishThrough(t, be, b, backend.ScanStored)

	// Break exactly what an aborted replace breaks: the recorded ids stop being live
	// children while the page's content still matches source.
	host := anchorHostRow(t, f)
	f.mu.Lock()
	for _, blk := range f.blocks[host] {
		blk["id"] = "reborn-" + blk["id"].(string)
	}
	f.mu.Unlock()

	scan, err := be.Scan(context.Background(), backend.ScanRecompute)
	if err != nil {
		t.Fatalf("recompute scan: %v", err)
	}
	if _, ok := scan.ContentHash(publish.NodeRef("CONTEXT.md")); ok {
		t.Error("a host with dangling anchors must not report a content hash that lets it hash-skip")
	}

	ops := publishThrough(t, be, b, backend.ScanRecompute)
	var reasserted bool
	for _, op := range ops {
		if op.Kind == graph.SetContent && op.Node == publish.NodeRef("CONTEXT.md") {
			reasserted = true
		}
	}
	if !reasserted {
		t.Errorf("host with dangling anchors was not re-asserted; ops = %v", ops)
	}

	// And the repair sticks: the anchors now resolve to blocks that are live.
	healed, err := be.Scan(context.Background(), backend.ScanRecompute)
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	live := liveChildIDs(t, f, host)
	for _, name := range anchorsCited(t, be, b) {
		id, ok := healed.AnchorID(name)
		if !ok {
			t.Errorf("anchor %s still unresolved after the repair", name)
			continue
		}
		if !live[string(id)] {
			t.Errorf("anchor %s resolves to %q, which is not a live child", name, id)
		}
	}
}

// anchorHostRow finds the row carrying the recorded anchor map.
func anchorHostRow(t *testing.T, f *fakeNotion) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		props, _ := row["properties"].(map[string]any)
		if props == nil {
			continue
		}
		if _, ok := props["anchors"]; ok {
			return row["id"].(string)
		}
	}
	t.Fatal("no row carries an anchors column; the publish recorded none")
	return ""
}

// liveChildIDs reports the block ids currently live under a page.
func liveChildIDs(t *testing.T, f *fakeNotion, pageID string) map[string]bool {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for _, blk := range f.blocks[pageID] {
		out[blk["id"].(string)] = true
	}
	return out
}
