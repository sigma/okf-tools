package transport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
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

	scan, err := be.Scan(context.Background(), backend.ScanStored)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	g, err := graph.Generate(context.Background(), b, scan)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	dag := optimize.Optimize(g, be, be)

	res, err := New(be).Run(context.Background(), dag, scan)
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

// ringBundle is a self-contained bundle of N new pages linked in a one-directional
// ring — a.md → b.md → … → a.md — all new against an empty scan. Unlike the
// reciprocal a↔b shape it has no mutual link anywhere, yet an unbounded/fusing bin
// still fuses each node's CreateNode+SetContent into one PackedTxn, so the ring of
// content-refs-node edges becomes a genuine N-cycle at the transaction layer. A
// fix that only breaks 2-cycles (reciprocal links) would miss this longer ring.
func ringBundle(rels ...string) map[string]string {
	files := map[string]string{
		"okf.toml":          "",
		"index.md":          "---\nokf_version: \"0.1\"\n---\n# Root\n",
		"docs/adr/index.md": "# ADRs\n",
	}
	for i, rel := range rels {
		next := rels[(i+1)%len(rels)] // last wraps back to first — closes the ring
		title := strings.ToUpper(rel[:1])
		files["docs/adr/"+rel+".md"] = "---\ntype: adr\ntitle: " + title +
			"\n---\nSee [next](" + next + ".md).\n"
	}
	return files
}

// publishUnbounded drives a bundle through the full generate → optimize →
// transport pipeline against a fresh UNBOUNDED (fusing) fake bin — fake.New() with
// no WithMaxCount. That is the whole point of these acceptance tests: maxCount=2 is
// the current workaround that seals each node's create separately from its content
// and hides the fusion cycle; dropping it reproduces the cycle a real fusing bin
// (Notion at its 100-block cap) exhibits. Returns the transport Result and error so
// callers assert the observable outcome only — keeping them strategy-independent.
func publishUnbounded(t *testing.T, files map[string]string) (*Result, error) {
	t.Helper()
	b := loadBundle(t, files)
	be := fake.New() // unbounded / fusing bin — no WithMaxCount

	scan, err := be.Scan(context.Background(), backend.ScanStored)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	g, err := graph.Generate(context.Background(), b, scan)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	dag := optimize.Optimize(g, be, be)
	return New(be).Run(context.Background(), dag, scan)
}

// TestEndToEndFusionCycles is the red acceptance test for the reciprocal-link
// fusion cycle (map sigma/okf-tools#66, ticket #68), routed by the strategy
// decision (#67) to cover BOTH shapes a general cycle-breaking fix must handle.
// Each case drives new, mutually-referencing pages through the full generate →
// optimize → transport pipeline with an UNBOUNDED (fusing) bin: the bin fuses each
// node's CreateNode+SetContent into a single PackedTxn, so the graph of
// content-refs-node edges becomes a genuine transaction cycle the transport drainer
// cannot sequence (transport.go:126, "unresolvable refs"). The existing
// pipeline/transport tests dodge this by forcing maxCount=2.
//
//   - reciprocal 2-cycle:   a ↔ b            (both new, each links the other)
//   - one-directional ring: a → b → c → a    (all new; a 2-cycle-only fix misses it)
//
// EXPECTED RED until the fusion fix ([T3]) lands: both cases assert a clean publish
// and currently fail at transport with the unresolvable-refs cycle error. When the
// fix lands they turn green with no change to this test.
func TestEndToEndFusionCycles(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		nodes []string // pages that must resolve once the cycle is broken
	}{
		{
			name:  "reciprocal_a_b",
			files: e2eBundle(), // docs/adr/a.md ↔ docs/adr/b.md, both new
			nodes: []string{"docs/adr/a.md", "docs/adr/b.md"},
		},
		{
			name:  "onedirectional_ring_a_b_c",
			files: ringBundle("a", "b", "c"), // a → b → c → a, all new
			nodes: []string{"docs/adr/a.md", "docs/adr/b.md", "docs/adr/c.md"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := publishUnbounded(t, tc.files)
			if err != nil {
				t.Fatalf("%s: new cross-linked pages must publish through the full "+
					"pipeline with an unbounded bin, but transport failed: %v", tc.name, err)
			}
			for _, rel := range tc.nodes {
				id := publish.SymbolicID("node:" + rel)
				if _, ok := res.Nodes[id]; !ok {
					t.Errorf("node %s never resolved — the fusion cycle blocked the drain", id)
				}
			}
		})
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
