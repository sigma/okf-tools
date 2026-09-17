package notion

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sigma/okf-tools/internal/bundle/bundletest"
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/optimize"
	"github.com/sigma/okf-tools/internal/publish/transport"
)

// The backend takes several transactions in flight at once (#212): the drain is
// otherwise bounded by Notion's per-request latency, not by the budget.
func TestNotionDeclaresConcurrency(t *testing.T) {
	var be backend.Executor = New()
	c, ok := be.(backend.ConcurrentExecutor)
	if !ok {
		t.Fatal("the Notion backend should declare a concurrency bound")
	}
	if c.Concurrency() < 2 {
		t.Errorf("Concurrency() = %d, want more than one in flight", c.Concurrency())
	}
}

// Two subpage groups under one owning row, completing concurrently, both land in
// the row's subtree map. Each write-back is a read-modify-write of the same
// column; unserialized, the second read would predate the first write and its
// PATCH would drop the other's entry — the stale-record failure of #189/#209 by
// another route.
func TestConcurrentWriteBacksToOneRowLoseNothing(t *testing.T) {
	b := bundletest.Load(t, map[string]string{
		"okf.toml":           "",
		"areas.json":         `{"docs": {"directory": "docs", "type": "doc"}}`,
		"index.md":           "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"docs/README.md":     "# Docs\n",
		"docs/dir/README.md": "---\ntype: doc\ntitle: Cluster\n---\nEntry.\n",
		"docs/dir/a.md":      "---\ntype: doc\ntitle: A\n---\nA.\n",
		"docs/dir/b.md":      "---\ntype: doc\ntitle: B\n---\nB.\n",
		"docs/dir/c.md":      "---\ntype: doc\ntitle: C\n---\nC.\n",
	})
	f := newFakeNotion()
	f.slowGet = 30 * time.Millisecond // long enough for the three merges to overlap
	be := newServer(t, f)

	if _, err := runPublish(t, be, b, backend.ScanStored); err != nil {
		t.Fatalf("publish: %v", err)
	}
	scan, err := be.Scan(context.Background(), backend.ScanStored)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, rel := range []string{"docs/dir/a.md", "docs/dir/b.md", "docs/dir/c.md"} {
		if _, ok := scan.NodeID(publish.NodeRef(rel)); !ok {
			t.Errorf("%s is missing from the README row's record after concurrent write-backs", rel)
		}
	}
}

// Concurrency changes the ORDER of the request stream, never its content: a
// publish drained eight at a time issues the same requests as one drained one at
// a time, and leaves the same mirror behind. (The one-at-a-time stream is the one
// every other offline test in this package pins its expectations to.)
func TestConcurrentPublishDoesTheSameWork(t *testing.T) {
	b := bundletest.Load(t, map[string]string{
		"okf.toml":           "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"areas.json":         `{"docs": {"directory": "docs", "type": "doc"}, "context": {"file": "CONTEXT.md", "type": "context", "role": "glossary"}}`,
		"index.md":           "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"CONTEXT.md":         "# Glossary\n\n**Root KEK**: the root key-encryption key.\n",
		"docs/README.md":     "# Docs\n",
		"docs/top.md":        "---\ntype: doc\ntitle: Top\n---\nSee [A](dir/a.md) and the [root KEK](../CONTEXT.md#root-kek).\n",
		"docs/dir/README.md": "---\ntype: doc\ntitle: Cluster\n---\nEntry.\n",
		"docs/dir/a.md":      "---\ntype: doc\ntitle: A\n---\nSee [B](b.md).\n",
		"docs/dir/b.md":      "---\ntype: doc\ntitle: B\n---\nSee [A](a.md).\n",
	})
	publish := func(t *testing.T, bound int) (map[string]int, []publish.SymbolicID) {
		t.Helper()
		f := newFakeNotion()
		be := newServer(t, f)
		ctx := context.Background()
		scan, err := be.Scan(ctx, backend.ScanStored)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		g, err := graph.Generate(ctx, b, scan, graph.WithHasher(be.RecomputeContentHasher(nil)))
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if _, err := transport.New(be, transport.WithConcurrency(bound)).Run(ctx, optimize.Optimize(g, be, be), scan); err != nil {
			t.Fatalf("publish at %d in flight: %v", bound, err)
		}
		f.mu.Lock()
		byShape := map[string]int{}
		for _, r := range f.reqs {
			byShape[r.Method+" "+routeShape(r.Path)]++
		}
		f.mu.Unlock()
		after, err := be.Scan(ctx, backend.ScanStored)
		if err != nil {
			t.Fatalf("scan after: %v", err)
		}
		var nodes []publish.SymbolicID
		for n := range after.Nodes() {
			nodes = append(nodes, n)
		}
		return byShape, nodes
	}

	seqReqs, seqNodes := publish(t, 1)
	conReqs, conNodes := publish(t, 8)
	if !maps.Equal(seqReqs, conReqs) {
		t.Errorf("request shapes differ:\n  one at a time: %v\n  eight:         %v", seqReqs, conReqs)
	}
	if !slices.Equal(seqNodes, conNodes) {
		t.Errorf("mirrors differ:\n  one at a time: %v\n  eight:         %v", seqNodes, conNodes)
	}
}

// routeShape replaces the ids in a request path with a placeholder, so two runs
// that mint different ids still describe the same request.
func routeShape(path string) string {
	seg := strings.Split(path, "/")
	for i, s := range seg {
		if strings.HasPrefix(s, "page-") || strings.HasPrefix(s, "block-") {
			seg[i] = "{id}"
		}
	}
	return strings.Join(seg, "/")
}
