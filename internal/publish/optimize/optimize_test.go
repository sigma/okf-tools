package optimize_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend/fake"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/optimize"
)

var update = flag.Bool("update", false, "update golden files")

// --- symbolic-id helpers (mirror the graph package's schemes) ---------------

func node(rel string) publish.SymbolicID    { return publish.SymbolicID("node:" + rel) }
func group(rel string) publish.GroupKey     { return publish.GroupKey("node:" + rel) }
func anchorRef(n string) publish.SymbolicID { return publish.SymbolicID("anchor:" + n) }

// block is a terse neutral block with optional refs/anchors.
func block(refs []publish.SymbolicID, anchors []publish.AnchorName) publish.Block {
	return publish.Block{Content: "x", Refs: refs, Anchors: anchors}
}

// representativeGraph hand-builds an op-DAG exercising every mechanism the
// optimizer must handle: fusion (create+props+content co-binning), overflow
// (content spilling past a bin into a second, edge back to the create),
// parent-before-child, a cross-node link, a glossary anchor host + a page citing
// it, an existing (scan-seeded) reference, and an orphan DeleteNode.
//
//	index.md   new, top-level parent index
//	a.md       new, child of index; content links node:b.md, cites anchor, +overflow
//	b.md       new, child of index (cross-node link target)
//	glossary.md new, hosts anchor "glossary/term"
//	old.md     orphan → DeleteNode (node id is scan-seeded)
func representativeGraph() *graph.Graph {
	g := &graph.Graph{}

	add := func(ops ...*graph.Op) { g.Ops = append(g.Ops, ops...) }

	// index.md — new top-level index.
	add(
		&graph.Op{Kind: graph.CreateNode, Node: node("index.md"), Parent: ""},
		&graph.Op{Kind: graph.SetProperties, Node: node("index.md")},
		&graph.Op{Kind: graph.SetContent, Node: node("index.md"), Doc: &publish.Document{
			Group:  group("index.md"),
			Blocks: []publish.Block{block(nil, nil)},
		}},
	)

	// a.md — new, parented under index; three content blocks so it overflows a
	// maxCount=2 bin. block1 links b.md, block2 cites the glossary anchor.
	add(
		&graph.Op{Kind: graph.CreateNode, Node: node("a.md"), Parent: node("index.md")},
		&graph.Op{Kind: graph.SetProperties, Node: node("a.md")},
		&graph.Op{Kind: graph.SetContent, Node: node("a.md"), Doc: &publish.Document{
			Group: group("a.md"),
			Blocks: []publish.Block{
				block([]publish.SymbolicID{node("b.md")}, nil),
				block([]publish.SymbolicID{anchorRef("glossary/term")}, nil),
				block(nil, nil),
			},
		}},
	)

	// b.md — new, parented under index; a plain content block.
	add(
		&graph.Op{Kind: graph.CreateNode, Node: node("b.md"), Parent: node("index.md")},
		&graph.Op{Kind: graph.SetProperties, Node: node("b.md")},
		&graph.Op{Kind: graph.SetContent, Node: node("b.md"), Doc: &publish.Document{
			Group:  group("b.md"),
			Blocks: []publish.Block{block(nil, nil)},
		}},
	)

	// glossary.md — new top-level host; its single content block hosts the anchor.
	add(
		&graph.Op{Kind: graph.CreateNode, Node: node("glossary.md"), Parent: ""},
		&graph.Op{Kind: graph.SetProperties, Node: node("glossary.md")},
		&graph.Op{Kind: graph.SetContent, Node: node("glossary.md"), Doc: &publish.Document{
			Group:  group("glossary.md"),
			Blocks: []publish.Block{block(nil, []publish.AnchorName{"glossary/term"})},
		}},
	)

	// old.md — orphan.
	add(&graph.Op{Kind: graph.DeleteNode, Node: node("old.md")})

	return g
}

// serialize renders a TxnDAG to a stable textual form for golden/determinism
// comparison. The opaque Txn is deliberately not inspected — only the
// backend-neutral PackedTxn metadata and the derived edges.
func serialize(d *optimize.TxnDAG) string {
	var b strings.Builder
	for i, t := range d.Txns {
		fmt.Fprintf(&b, "txn %d group=%s produces=%s anchors=%s refs=%s\n",
			i, t.Group, ids(t.Produces), anchors(t.Anchors), ids(t.Refs))
	}
	for _, e := range d.Edges {
		fmt.Fprintf(&b, "edge %d->%d\n", e.From, e.To)
	}
	return b.String()
}

func ids(in []publish.SymbolicID) string {
	s := make([]string, len(in))
	for i, v := range in {
		s[i] = string(v)
	}
	return "[" + strings.Join(s, " ") + "]"
}

func anchors(in []publish.AnchorName) string {
	s := make([]string, len(in))
	for i, v := range in {
		s[i] = string(v)
	}
	return "[" + strings.Join(s, " ") + "]"
}

// TestOptimizeGolden pins the full transaction-DAG of the representative fixture
// against a golden file, under packing pressure (maxCount=2) so fusion, overflow,
// and every edge cause appear.
func TestOptimizeGolden(t *testing.T) {
	be := fake.New(fake.WithMaxCount(2))
	got := serialize(optimize.Optimize(representativeGraph(), be, be))

	golden := filepath.Join("testdata", "representative.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("transaction-DAG mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestOptimizeDeterministic enforces the ratified determinism property: two runs
// on the same fixture graph and backend yield byte-identical transaction-DAGs.
func TestOptimizeDeterministic(t *testing.T) {
	g := representativeGraph()
	be := fake.New(fake.WithMaxCount(2))

	first := serialize(optimize.Optimize(g, be, be))
	second := serialize(optimize.Optimize(g, be, be))
	if first != second {
		t.Errorf("Optimize is not deterministic\n--- run 1 ---\n%s\n--- run 2 ---\n%s", first, second)
	}
}

// findProducer returns the index of the sole txn that Produces id, or -1.
func findProducer(d *optimize.TxnDAG, id publish.SymbolicID) int {
	for i, t := range d.Txns {
		if slices.Contains(t.Produces, id) {
			return i
		}
	}
	return -1
}

func hasEdge(d *optimize.TxnDAG, from, to int) bool {
	return slices.Contains(d.Edges, optimize.Edge{From: from, To: to})
}

// TestFusionSuppressesWriteTarget: with an unbounded bin, a node's create, props,
// and content all co-bin, so its own node id is Produced and thus suppressed from
// the exposed Refs — no self-edge, fusion realized.
func TestFusionSuppressesWriteTarget(t *testing.T) {
	g := &graph.Graph{Ops: []*graph.Op{
		{Kind: graph.CreateNode, Node: node("a.md")},
		{Kind: graph.SetProperties, Node: node("a.md")},
		{Kind: graph.SetContent, Node: node("a.md"), Doc: &publish.Document{
			Group:  group("a.md"),
			Blocks: []publish.Block{block(nil, nil), block(nil, nil)},
		}},
	}}
	be := fake.New() // unbounded

	d := optimize.Optimize(g, be, be)
	if len(d.Txns) != 1 {
		t.Fatalf("want 1 fused txn, got %d", len(d.Txns))
	}
	if got := d.Txns[0].Refs; len(got) != 0 {
		t.Errorf("write-target node:a.md should be suppressed, exposed Refs = %v", got)
	}
	if !slices.Contains(d.Txns[0].Produces, node("a.md")) {
		t.Errorf("fused txn should Produce node:a.md, got %v", d.Txns[0].Produces)
	}
	if len(d.Edges) != 0 {
		t.Errorf("fused single txn should have no edges, got %v", d.Edges)
	}
}

// acyclic reports whether the transaction-DAG has no directed cycle, via a Kahn
// topological drain: repeatedly remove a zero-indegree txn until none remain. A
// leftover node means a cycle. This is the property the transport drainer needs —
// a cyclic txn-DAG has a wavefront that never becomes ready.
func acyclic(d *optimize.TxnDAG) bool {
	indeg := make([]int, len(d.Txns))
	out := make([][]int, len(d.Txns))
	for _, e := range d.Edges {
		indeg[e.To]++
		out[e.From] = append(out[e.From], e.To)
	}
	var queue []int
	for i, n := range indeg {
		if n == 0 {
			queue = append(queue, i)
		}
	}
	drained := 0
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		drained++
		for _, w := range out[v] {
			indeg[w]--
			if indeg[w] == 0 {
				queue = append(queue, w)
			}
		}
	}
	return drained == len(d.Txns)
}

// linkGraph builds an op-DAG of new pages whose content links form the edges in
// links (rel → rel): each page is a CreateNode + a single content block citing its
// target's node id. It is the minimal shape that fuses into a transaction cycle
// under an unbounded bin.
func linkGraph(links map[string]string) *graph.Graph {
	g := &graph.Graph{}
	rels := make([]string, 0, len(links))
	for rel := range links {
		rels = append(rels, rel)
	}
	slices.Sort(rels)
	for _, rel := range rels {
		g.Ops = append(g.Ops,
			&graph.Op{Kind: graph.CreateNode, Node: node(rel)},
			&graph.Op{Kind: graph.SetContent, Node: node(rel), Doc: &publish.Document{
				Group:  group(rel),
				Blocks: []publish.Block{block([]publish.SymbolicID{node(links[rel])}, nil)},
			}},
		)
	}
	return g
}

// TestScopedSealBreaksFusionCycle is the optimizer-level guard for strategy #67:
// with an unbounded (fusing) bin, mutually-linking NEW pages would fuse each
// node's create+content into one PackedTxn and close a transaction cycle. Optimize
// must detect that on the real fused DAG and force-split the minimal node set so
// the emitted txn-DAG is acyclic — for a reciprocal 2-cycle and a one-directional
// ring alike — while still producing every node exactly once and staying
// deterministic across runs.
func TestScopedSealBreaksFusionCycle(t *testing.T) {
	cases := []struct {
		name  string
		links map[string]string
		nodes []string
	}{
		{
			name:  "reciprocal_2_cycle",
			links: map[string]string{"a.md": "b.md", "b.md": "a.md"},
			nodes: []string{"a.md", "b.md"},
		},
		{
			name:  "onedirectional_3_ring",
			links: map[string]string{"a.md": "b.md", "b.md": "c.md", "c.md": "a.md"},
			nodes: []string{"a.md", "b.md", "c.md"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := linkGraph(tc.links)
			be := fake.New() // unbounded: absent the fix, every node fuses into a cycle

			d := optimize.Optimize(g, be, be)
			if !acyclic(d) {
				t.Errorf("txn-DAG still cyclic after the scoped-seal loop:\n%s", serialize(d))
			}
			// Every node must still be produced exactly once (splitting a create from
			// its content must not drop or duplicate the create).
			for _, rel := range tc.nodes {
				if idx := findProducer(d, node(rel)); idx < 0 {
					t.Errorf("no txn Produces %s after the split", node(rel))
				}
			}
			// Determinism (#164 §6): the split selection is a total order on Group id,
			// so a second run is byte-identical.
			if got, want := serialize(optimize.Optimize(g, be, be)), serialize(d); got != want {
				t.Errorf("cycle-breaking is nondeterministic\n--- run 2 ---\n%s\n--- run 1 ---\n%s", got, want)
			}
		})
	}
}

// TestScopedSealCostIsMinimal pins the cost bound from #67: breaking a reciprocal
// a↔b cycle costs exactly ONE extra transaction — a single node is de-fused
// (its create+content split into two txns), the other stays fused. Absent a cycle
// the seam never fires, so a fused pair is two txns and a broken pair is three.
func TestScopedSealCostIsMinimal(t *testing.T) {
	g := linkGraph(map[string]string{"a.md": "b.md", "b.md": "a.md"})
	be := fake.New() // unbounded

	d := optimize.Optimize(g, be, be)
	// a.md and b.md, unbounded and mutually linked: one splits into {create}+{content}
	// (2 txns), the other stays fused (1 txn) → 3 total.
	if got := len(d.Txns); got != 3 {
		t.Errorf("want 3 txns (one node de-fused, one fused), got %d:\n%s", got, serialize(d))
	}
	// The lowest Group id (node:a.md) is the one force-split, so it owns two txns.
	aTxns := 0
	for _, tx := range d.Txns {
		if tx.Group == group("a.md") {
			aTxns++
		}
	}
	if aTxns != 2 {
		t.Errorf("lowest-id node a.md should be the one de-fused (2 txns), got %d", aTxns)
	}
}

// TestOverflowEdge: with maxCount=2, a.md's create+props fill bin 1 and its
// content overflows into bin 2, which then exposes node:a.md (not produced there)
// and gets an edge from the create bin.
func TestOverflowEdge(t *testing.T) {
	g := &graph.Graph{Ops: []*graph.Op{
		{Kind: graph.CreateNode, Node: node("a.md")},
		{Kind: graph.SetProperties, Node: node("a.md")},
		{Kind: graph.SetContent, Node: node("a.md"), Doc: &publish.Document{
			Group:  group("a.md"),
			Blocks: []publish.Block{block(nil, nil), block(nil, nil)},
		}},
	}}
	be := fake.New(fake.WithMaxCount(2))

	d := optimize.Optimize(g, be, be)
	if len(d.Txns) != 2 {
		t.Fatalf("want 2 txns (create-bin + overflow), got %d", len(d.Txns))
	}
	createIdx := findProducer(d, node("a.md"))
	if createIdx < 0 {
		t.Fatal("no txn Produces node:a.md")
	}
	overflow := 1 - createIdx
	if !slices.Contains(d.Txns[overflow].Refs, node("a.md")) {
		t.Errorf("overflow txn should expose node:a.md, Refs = %v", d.Txns[overflow].Refs)
	}
	if !hasEdge(d, createIdx, overflow) {
		t.Errorf("want overflow edge %d->%d, edges = %v", createIdx, overflow, d.Edges)
	}
}

// TestCrossNodeAndAnchorEdges: node and anchor refs wire edges to their producers.
func TestCrossNodeAndAnchorEdges(t *testing.T) {
	g := &graph.Graph{Ops: []*graph.Op{
		// consumer page cites node:b.md and anchor glossary/term.
		{Kind: graph.SetContent, Node: node("a.md"), Doc: &publish.Document{
			Group: group("a.md"),
			Blocks: []publish.Block{
				block([]publish.SymbolicID{node("b.md"), anchorRef("glossary/term")}, nil),
			},
		}},
		// b.md is created this run.
		{Kind: graph.CreateNode, Node: node("b.md")},
		// glossary hosts the anchor via a content block.
		{Kind: graph.SetContent, Node: node("g.md"), Doc: &publish.Document{
			Group:  group("g.md"),
			Blocks: []publish.Block{block(nil, []publish.AnchorName{"glossary/term"})},
		}},
	}}
	be := fake.New() // unbounded: one txn per group

	d := optimize.Optimize(g, be, be)
	aIdx := slices.IndexFunc(d.Txns, func(t publish.PackedTxn) bool { return t.Group == group("a.md") })
	bIdx := findProducer(d, node("b.md"))
	if aIdx < 0 || bIdx < 0 {
		t.Fatalf("missing txns: a=%d b=%d", aIdx, bIdx)
	}
	if !hasEdge(d, bIdx, aIdx) {
		t.Errorf("want cross-node edge b(%d)->a(%d), edges = %v", bIdx, aIdx, d.Edges)
	}
	gIdx := slices.IndexFunc(d.Txns, func(t publish.PackedTxn) bool {
		return slices.Contains(t.Anchors, publish.AnchorName("glossary/term"))
	})
	if gIdx < 0 {
		t.Fatal("no txn hosts the anchor")
	}
	if !hasEdge(d, gIdx, aIdx) {
		t.Errorf("want anchor edge g(%d)->a(%d), edges = %v", gIdx, aIdx, d.Edges)
	}
}

// TestScanSeededRefNoEdge: a ref whose target is neither Produced nor Anchored by
// any txn this run (it exists in the scan) stays exposed but wires no edge.
func TestScanSeededRefNoEdge(t *testing.T) {
	g := &graph.Graph{Ops: []*graph.Op{
		// a.md changed (no CreateNode) and links b.md, which already exists.
		{Kind: graph.SetContent, Node: node("a.md"), Doc: &publish.Document{
			Group:  group("a.md"),
			Blocks: []publish.Block{block([]publish.SymbolicID{node("b.md")}, nil)},
		}},
	}}
	be := fake.New()

	d := optimize.Optimize(g, be, be)
	if len(d.Edges) != 0 {
		t.Errorf("scan-seeded ref should wire no edge, got %v", d.Edges)
	}
	if !slices.Contains(d.Txns[0].Refs, node("b.md")) {
		t.Errorf("scan-seeded ref should still be exposed for transport gating, Refs = %v", d.Txns[0].Refs)
	}
}

// TestDeleteExposesTarget: a DeleteNode exposes its (scan-seeded) node id but,
// with no producer this run, wires no edge.
func TestDeleteExposesTarget(t *testing.T) {
	g := &graph.Graph{Ops: []*graph.Op{
		{Kind: graph.DeleteNode, Node: node("old.md")},
	}}
	be := fake.New()

	d := optimize.Optimize(g, be, be)
	if len(d.Txns) != 1 || len(d.Edges) != 0 {
		t.Fatalf("want 1 txn / 0 edges, got %d txns, edges %v", len(d.Txns), d.Edges)
	}
	if !slices.Contains(d.Txns[0].Refs, node("old.md")) {
		t.Errorf("delete should expose node:old.md, Refs = %v", d.Txns[0].Refs)
	}
	if len(d.Txns[0].Produces) != 0 {
		t.Errorf("delete produces nothing, got %v", d.Txns[0].Produces)
	}
}

// TestParentBeforeChildEdge: a child create referencing a parent created this run
// wires a parent-before-child edge.
func TestParentBeforeChildEdge(t *testing.T) {
	g := &graph.Graph{Ops: []*graph.Op{
		{Kind: graph.CreateNode, Node: node("index.md"), Parent: ""},
		{Kind: graph.CreateNode, Node: node("child.md"), Parent: node("index.md")},
	}}
	be := fake.New()

	d := optimize.Optimize(g, be, be)
	parent := findProducer(d, node("index.md"))
	child := findProducer(d, node("child.md"))
	if parent < 0 || child < 0 {
		t.Fatalf("missing create txns: parent=%d child=%d", parent, child)
	}
	if !hasEdge(d, parent, child) {
		t.Errorf("want parent-before-child edge %d->%d, edges = %v", parent, child, d.Edges)
	}
}
