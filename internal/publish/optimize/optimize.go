// Package optimize is Stage 2 of the okfpub pipeline: a pure, deterministic
// transform from the op-DAG into a transaction-DAG.
//
// Optimize(opDAG, Tokenizer, ConstraintModel) → txnDAG owns the Tokenize call
// (its input is the neutral op-DAG, so it is the stage that turns neutral ops
// into packable AtomicUnits), partitions units by Group (identity partition),
// packs each group next-fit over a create-before-content stable order, seals each
// bin into a PackedTxn, and derives the transaction-DAG's edges mechanically. It
// depends on exactly two backend roles — Tokenizer + ConstraintModel — and no
// transport/runtime state, so it is golden-file testable and its dry-runs are
// reproducible.
//
// The correctness crux (#164 §5): a unit's own write-target is one of its Refs,
// and a PackedTxn's exposed Refs are the union of its units' Refs minus what the
// same transaction Produces/Anchors (intra-transaction Ref-suppression). Fusion,
// overflow-ordering, and cross-node links then all fall out with zero new edge
// causes and zero fusion logic here.
//
// See sigma/ideas#172 (ratified #164).
package optimize

import (
	"cmp"
	"slices"
	"strings"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/graph"
)

// TxnDAG is the transaction-DAG Optimize emits: the packed transactions and the
// "must complete before" dependency edges between them. It is the uniform
// artifact the transport drains wavefront-by-wavefront.
type TxnDAG struct {
	// Txns are the packed transactions, in deterministic order (by Group, then by
	// the create-before-content packing order within a group).
	Txns []publish.PackedTxn
	// Edges are the derived dependency edges, referencing Txns by index.
	Edges []Edge
}

// Edge is a "must complete before" dependency between two packed transactions,
// referenced by their index into TxnDAG.Txns: Txns[From] must complete before
// Txns[To]. It is derived mechanically — To depends on From iff a (post-
// suppression) Ref of Txns[To] is produced by Txns[From] (a node in Produces or
// an anchor in Anchors) — which reduces exactly to the op-graph's three causes
// lifted to transactions.
type Edge struct {
	From int
	To   int
}

// packing phases order a group's units create-before-content so a fusible run
// (CreateNode → SetProperties → SetContent chunks for one node) is presented to
// the Bin consecutively. DeleteNode is a standalone reconcile op and sorts last.
const (
	phaseCreate = iota
	phaseProps
	phaseContent
	phaseDelete
)

// taggedUnit is an AtomicUnit plus the optimizer-side provenance the AtomicUnit
// itself does not carry: what the unit produces (a CreateNode's minted node),
// which node it concerns (for ordering), and its create-before-content phase and
// stable sequence.
type taggedUnit struct {
	unit     publish.AtomicUnit
	produces []publish.SymbolicID
	target   publish.SymbolicID
	phase    int
	seq      int
	// hash, propHash, parent, and title are the write-back provenance carried down
	// from the op: the node's expected content and property hashes, its parent
	// symbolic id, and its display title. They ride every unit of a touched node so
	// the sealed PackedTxn can expose them regardless of which unit (create / props
	// / content chunk) landed in the bin.
	hash     publish.Hash
	propHash publish.Hash
	parent   publish.SymbolicID
	title    string
}

// Optimize turns the op-DAG into a transaction-DAG. It is a pure function of the
// graph structure and the backend's Tokenizer + ConstraintModel — no transport or
// runtime state — and is deterministic: the same graph and backend yield a
// byte-identical TxnDAG.
//
// Fusion can collapse a node's CreateNode+SetContent into one PackedTxn, so a
// directed cycle among mutually-linking new pages (reciprocal a↔b, or a longer
// ring a→b→c→a) fuses into a transaction cycle the transport cannot drain. The
// scoped-seal loop below (strategy sigma/okf-tools#67) detects cycles on the
// *actual* fused txn-DAG and de-fuses the minimal node set: each round it force-
// splits the lowest-Group-id node of every cyclic SCC (sealing its create[+props]
// away from its content, so the node's existence is produced by a txn carrying no
// outbound content Refs), then re-packs and re-derives until the DAG is acyclic.
// Detecting on the post-pack DAG accounts for overflow for free — a node already
// split by capacity cannot sit on a fusion cycle — and only cross-linking bundles
// pay the one extra transaction; healthy-fusion bundles are untouched.
func Optimize(g *graph.Graph, tk backend.Tokenizer, cm backend.ConstraintModel) *TxnDAG {
	units := tokenize(g, tk)

	// forceSplit accumulates the Groups whose create-from-content seam must be
	// sealed to break a fusion cycle. It only ever grows, and split selection is a
	// total order on Group id, so the loop is deterministic (#164 §6).
	forceSplit := map[publish.GroupKey]bool{}
	txns := pack(units, cm, forceSplit)
	edges := deriveEdges(txns)
	// Every round splits at least one previously-unsplit node, so the loop runs at
	// most once per distinct Group; the bound is a belt-and-suspenders backstop
	// against a non-terminating detect/re-pack cycle that the acyclicity invariant
	// says cannot happen.
	for round := 0; round <= len(txns); round++ {
		cyclic := cyclicSplitTargets(txns, edges, forceSplit)
		if len(cyclic) == 0 {
			return &TxnDAG{Txns: txns, Edges: edges}
		}
		for _, grp := range cyclic {
			forceSplit[grp] = true
		}
		txns = pack(units, cm, forceSplit)
		edges = deriveEdges(txns)
	}
	panic("optimize: transaction-DAG still cyclic after splitting every cyclic node")
}

// tokenize turns each op into AtomicUnits: it owns the Tokenize call for
// SetContent (each block-level unit gains its node as a write-target Ref, #164
// §5), and routes CreateNode / SetProperties / DeleteNode through the backend's
// TokenizeOp to mint one unit each. The returned units carry a stable, monotonic
// seq so ties break deterministically.
//
// All four op types flow through the backend so each unit carries a real backend
// Payload and Cost — that is what lets a fusing backend (Notion collapsing create
// + props + first content into one POST /pages) recognise and fuse them inside its
// Bin. The optimizer stays backend-agnostic: it forwards a neutral NonContentOp
// (never building a backend payload) and then stamps the unit's neutral Group,
// write-target/parent Refs, and provenance itself. Against the count-based fake
// backend (empty payloads) this is exactly the previous behaviour.
func tokenize(g *graph.Graph, tk backend.Tokenizer) []taggedUnit {
	var out []taggedUnit
	seq := 0
	next := func() int { s := seq; seq++; return s }

	for _, op := range g.Ops {
		switch op.Kind {
		case graph.CreateNode:
			u := tk.TokenizeOp(publish.NonContentOp{Kind: publish.CreateOp, Node: op.Node})
			u.Group = publish.GroupKey(op.Node)
			if op.Parent != "" {
				// Parent-before-child is just the create-unit referencing its parent.
				u.Refs = []publish.SymbolicID{op.Parent}
			}
			out = append(out, taggedUnit{
				unit:     u,
				produces: []publish.SymbolicID{op.Node},
				target:   op.Node,
				phase:    phaseCreate,
				seq:      next(),
				hash:     op.Hash,
				propHash: op.PropHash,
				parent:   op.Parent,
				title:    op.Title,
			})
		case graph.SetProperties:
			// SetProperties(A) carries Ref{node:A} — its write target (#164 §5). The
			// backend turns op.Props into its own property payload.
			u := tk.TokenizeOp(publish.NonContentOp{Kind: publish.PropertiesOp, Node: op.Node, Props: op.Props})
			u.Group = publish.GroupKey(op.Node)
			u.Refs = []publish.SymbolicID{op.Node}
			out = append(out, taggedUnit{
				unit:     u,
				target:   op.Node,
				phase:    phaseProps,
				seq:      next(),
				hash:     op.Hash,
				propHash: op.PropHash,
				parent:   op.Parent,
				title:    op.Title,
			})
		case graph.SetContent:
			if op.Doc == nil {
				continue
			}
			for _, ru := range tk.Tokenize(*op.Doc) {
				target := publish.SymbolicID(ru.Group)
				// Add the node's own id as a write-target Ref alongside the unit's
				// outbound Refs, without mutating the tokenizer's returned slice.
				refs := make([]publish.SymbolicID, 0, len(ru.Refs)+1)
				refs = append(refs, ru.Refs...)
				refs = append(refs, target)
				ru.Refs = refs
				out = append(out, taggedUnit{
					unit:     ru,
					target:   target,
					phase:    phaseContent,
					seq:      next(),
					hash:     op.Hash,
					propHash: op.PropHash,
					parent:   op.Parent,
					title:    op.Title,
				})
			}
		case graph.DeleteNode:
			// A DeleteNode needs the (scan-seeded) node id to archive it: its write
			// target as a Ref. No producer emits node:A this run, so it stays exposed
			// and the transport resolves it from the scan seed — no edge.
			u := tk.TokenizeOp(publish.NonContentOp{Kind: publish.DeleteOp, Node: op.Node})
			u.Group = publish.GroupKey(op.Node)
			u.Refs = []publish.SymbolicID{op.Node}
			out = append(out, taggedUnit{
				unit:   u,
				target: op.Node,
				phase:  phaseDelete,
				seq:    next(),
			})
		}
	}
	return out
}

// pack applies the identity partition on Group and, within each group, next-fit
// packs the create-before-content ordered units into bins, sealing each bin into
// a PackedTxn with intra-transaction Ref-suppression. Groups are visited in
// sorted GroupKey order for determinism. A group in forceSplit has its
// create[+props] prefix sealed away from its content (the scoped seal that breaks
// a fusion cycle).
func pack(units []taggedUnit, cm backend.ConstraintModel, forceSplit map[publish.GroupKey]bool) []publish.PackedTxn {
	byGroup := map[publish.GroupKey][]taggedUnit{}
	for _, tu := range units {
		byGroup[tu.unit.Group] = append(byGroup[tu.unit.Group], tu)
	}
	groups := make([]publish.GroupKey, 0, len(byGroup))
	for g := range byGroup {
		groups = append(groups, g)
	}
	slices.Sort(groups)

	var txns []publish.PackedTxn
	for _, g := range groups {
		gu := byGroup[g]
		// Stable order: node-by-node, create-before-content, tie-broken by the
		// stable sequence (which preserves each SetContent's block order).
		slices.SortFunc(gu, func(a, b taggedUnit) int {
			if a.target != b.target {
				return strings.Compare(string(a.target), string(b.target))
			}
			if a.phase != b.phase {
				return a.phase - b.phase
			}
			return a.seq - b.seq
		})
		txns = append(txns, packGroup(g, gu, cm, forceSplit[g])...)
	}
	return txns
}

// packGroup next-fit packs one group's ordered units: a single open bin accepts
// units until it refuses, then it is sealed and a fresh bin opened. Each sealed
// bin becomes a PackedTxn.
//
// When forceSplit is set, the open bin is additionally sealed at the create→content
// boundary — before the group's first content unit lands — so the node's
// existence (its CreateNode, and any SetProperties, all of which expose only the
// node's own self-produced write-target Ref) seals into a txn distinct from its
// content, which alone carries the outbound cross-node Refs that formed the cycle.
// The seam fires at most once (the create[+props] prefix is contiguous and sorts
// before content); content then packs normally, overflow and all.
func packGroup(g publish.GroupKey, gu []taggedUnit, cm backend.ConstraintModel, forceSplit bool) []publish.PackedTxn {
	var txns []publish.PackedTxn
	bin := cm.NewBin()
	acc := newAccumulator(g)
	filled := false       // whether the open bin holds at least one unit
	sealedPrefix := false // whether the forced create→content seal has already fired

	for _, tu := range gu {
		// Forced create→content seal: the first content unit of a flagged group
		// opens a fresh bin, sealing whatever create[+props] units preceded it. If
		// no such prefix landed (a content-only update to an existing node), there
		// is nothing to split and the seam is a no-op.
		if forceSplit && !sealedPrefix && tu.phase == phaseContent {
			sealedPrefix = true
			if filled {
				txns = append(txns, acc.seal(bin.Build()))
				bin = cm.NewBin()
				acc = newAccumulator(g)
				filled = false
			}
		}
		if bin.Add(tu.unit) {
			acc.add(tu)
			filled = true
			continue
		}
		// The open bin refused the unit. A refusal on an empty bin means the unit
		// fits no bin at all — a broken ConstraintModel contract (every backend
		// must accept a lone unit), which we fail loudly rather than spin or emit a
		// transaction whose sealed payload and metadata disagree.
		if !filled {
			panic("optimize: ConstraintModel refused a unit on an empty bin")
		}
		// Seal the full bin, open a fresh one, and place the unit there.
		txns = append(txns, acc.seal(bin.Build()))
		bin = cm.NewBin()
		acc = newAccumulator(g)
		if !bin.Add(tu.unit) {
			panic("optimize: ConstraintModel refused a unit on an empty bin")
		}
		acc.add(tu)
		filled = true
	}
	if filled {
		txns = append(txns, acc.seal(bin.Build()))
	}
	return txns
}

// accumulator gathers the backend-neutral metadata of one bin's worth of units so
// the sealed Transaction can be wrapped in a PackedTxn.
type accumulator struct {
	group    publish.GroupKey
	refs     []publish.SymbolicID
	produces []publish.SymbolicID
	anchors  []publish.AnchorName
	// hash, propHash, parent, and title are the write-back provenance of the bin's
	// node. Every unit of one node carries the same values; the accumulator keeps the
	// first non-empty it sees (a content-less node still carries them via its
	// props/create unit).
	hash     publish.Hash
	propHash publish.Hash
	parent   publish.SymbolicID
	title    string
}

func newAccumulator(g publish.GroupKey) *accumulator {
	return &accumulator{group: g}
}

func (a *accumulator) add(tu taggedUnit) {
	a.refs = append(a.refs, tu.unit.Refs...)
	a.anchors = append(a.anchors, tu.unit.Anchors...)
	a.produces = append(a.produces, tu.produces...)
	if a.hash == "" {
		a.hash = tu.hash
	}
	if a.propHash == "" {
		a.propHash = tu.propHash
	}
	if a.parent == "" {
		a.parent = tu.parent
	}
	if a.title == "" {
		a.title = tu.title
	}
}

// seal wraps the built Transaction with its aggregated metadata, applying
// intra-transaction Ref-suppression: exposed Refs are the union of the units'
// Refs minus anything this same transaction Produces (a node id) or Anchors (an
// anchor id). All slices are deduplicated and sorted so the output is
// byte-deterministic.
func (a *accumulator) seal(txn publish.Transaction) publish.PackedTxn {
	produced := map[publish.SymbolicID]bool{}
	for _, id := range a.produces {
		produced[id] = true
	}
	hosted := map[publish.AnchorName]bool{}
	for _, name := range a.anchors {
		hosted[name] = true
	}

	var exposed []publish.SymbolicID
	for _, ref := range a.refs {
		if produced[ref] {
			continue // self-satisfied node ref (e.g. fusion)
		}
		if name, ok := ref.AnchorName(); ok && hosted[name] {
			continue // self-satisfied anchor ref
		}
		exposed = append(exposed, ref)
	}

	return publish.PackedTxn{
		Txn:      txn,
		Group:    a.group,
		Refs:     dedupSorted(exposed),
		Anchors:  dedupSorted(a.anchors),
		Produces: dedupSorted(a.produces),
		Hash:     a.hash,
		PropHash: a.propHash,
		Parent:   a.parent,
		Title:    a.title,
	}
}

// deriveEdges wires the transaction-DAG mechanically: Txns[To] depends on
// Txns[From] iff a post-suppression Ref of To is produced by From — a node id in
// From.Produces or an anchor id in From.Anchors. A Ref with no producer this run
// is scan-seeded and gets no edge. Edges are deduplicated and sorted.
func deriveEdges(txns []publish.PackedTxn) []Edge {
	nodeProducer := map[publish.SymbolicID]int{}
	anchorProducer := map[publish.AnchorName]int{}
	for i := range txns {
		for _, id := range txns[i].Produces {
			nodeProducer[id] = i
		}
		for _, name := range txns[i].Anchors {
			anchorProducer[name] = i
		}
	}

	seen := map[Edge]bool{}
	var edges []Edge
	for to := range txns {
		for _, ref := range txns[to].Refs {
			from := -1
			if name, ok := ref.AnchorName(); ok {
				if p, has := anchorProducer[name]; has {
					from = p
				}
			} else if p, has := nodeProducer[ref]; has {
				from = p
			}
			if from < 0 || from == to {
				continue
			}
			e := Edge{From: from, To: to}
			if !seen[e] {
				seen[e] = true
				edges = append(edges, e)
			}
		}
	}
	slices.SortFunc(edges, func(a, b Edge) int {
		if a.From != b.From {
			return a.From - b.From
		}
		return a.To - b.To
	})
	return edges
}

// cyclicSplitTargets finds the strongly-connected components of the transaction-
// DAG and, for each one that is an actual cycle, returns the Group of the node to
// force-split to help break it: the lowest Group id among the SCC's txns not
// already split. The lowest-id rule is a total order, so the choice — and thus the
// whole optimize loop — is deterministic (#164 §6); it is deliberately not a
// minimal feedback-vertex-set, since correctness and determinism matter at this
// scale and optimality does not. Every txn in a cyclic SCC Produces a node (a txn
// producing nothing has no inbound edge and cannot close a cycle), so a valid,
// splittable Group always exists. The returned Groups are sorted and unique.
func cyclicSplitTargets(txns []publish.PackedTxn, edges []Edge, forceSplit map[publish.GroupKey]bool) []publish.GroupKey {
	adj := make([][]int, len(txns))
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], e.To)
	}

	// Tarjan's SCC. Graphs are tiny (a handful of new pages), so plain recursion
	// over a fixed adjacency is ample.
	const unvisited = -1
	index := make([]int, len(txns))
	low := make([]int, len(txns))
	onStack := make([]bool, len(txns))
	for i := range index {
		index[i] = unvisited
	}
	var stack []int
	next := 0
	var targets []publish.GroupKey

	// record inspects a finished SCC: an SCC is a cycle iff it has more than one
	// node (self-edges are never derived). It picks the lowest unsplit Group.
	record := func(comp []int) {
		if len(comp) < 2 {
			return
		}
		chosen := publish.GroupKey("")
		for _, v := range comp {
			g := txns[v].Group
			if forceSplit[g] {
				continue
			}
			if chosen == "" || g < chosen {
				chosen = g
			}
		}
		if chosen != "" {
			targets = append(targets, chosen)
		}
	}

	var strongConnect func(v int)
	strongConnect = func(v int) {
		index[v] = next
		low[v] = next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range adj[v] {
			if index[w] == unvisited {
				strongConnect(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			var comp []int
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			record(comp)
		}
	}
	for v := range txns {
		if index[v] == unvisited {
			strongConnect(v)
		}
	}

	slices.Sort(targets)
	return slices.Compact(targets)
}

// dedupSorted returns a sorted, duplicate-free clone of ids so a PackedTxn's id
// slices are byte-deterministic regardless of unit encounter order.
func dedupSorted[T cmp.Ordered](ids []T) []T {
	if len(ids) == 0 {
		return nil
	}
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}
