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
}

// Optimize turns the op-DAG into a transaction-DAG. It is a pure function of the
// graph structure and the backend's Tokenizer + ConstraintModel — no transport or
// runtime state — and is deterministic: the same graph and backend yield a
// byte-identical TxnDAG.
func Optimize(g *graph.Graph, tk backend.Tokenizer, cm backend.ConstraintModel) *TxnDAG {
	units := tokenize(g, tk)
	txns := pack(units, cm)
	edges := deriveEdges(txns)
	return &TxnDAG{Txns: txns, Edges: edges}
}

// tokenize turns each op into AtomicUnits: it owns the Tokenize call for
// SetContent (each block-level unit gains its node as a write-target Ref, #164
// §5), and maps CreateNode / SetProperties / DeleteNode to one unit each. The
// returned units carry a stable, monotonic seq so ties break deterministically.
//
// Only SetContent flows through the backend's Tokenizer; the other three ops
// carry no backend Payload or Cost here (their semantics live on the Op —
// Parent, Props, Node). Against the count-based fake backend that is exact. A
// real fusing backend (Notion collapsing create + props + first content into one
// POST /pages) will need the create/props units to carry a backend create-block
// and a real Cost, which is a Tokenizer-contract concern for that backend, not
// the optimizer's — see the ticket note.
func tokenize(g *graph.Graph, tk backend.Tokenizer) []taggedUnit {
	var out []taggedUnit
	seq := 0
	next := func() int { s := seq; seq++; return s }

	for _, op := range g.Ops {
		switch op.Kind {
		case graph.CreateNode:
			u := publish.AtomicUnit{Group: publish.GroupKey(op.Node)}
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
			})
		case graph.SetProperties:
			// SetProperties(A) carries Ref{node:A} — its write target (#164 §5).
			out = append(out, taggedUnit{
				unit: publish.AtomicUnit{
					Payload: op.Props,
					Group:   publish.GroupKey(op.Node),
					Refs:    []publish.SymbolicID{op.Node},
				},
				target: op.Node,
				phase:  phaseProps,
				seq:    next(),
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
					unit:   ru,
					target: target,
					phase:  phaseContent,
					seq:    next(),
				})
			}
		case graph.DeleteNode:
			// A DeleteNode needs the (scan-seeded) node id to archive it: its write
			// target as a Ref. No producer emits node:A this run, so it stays exposed
			// and the transport resolves it from the scan seed — no edge.
			out = append(out, taggedUnit{
				unit: publish.AtomicUnit{
					Group: publish.GroupKey(op.Node),
					Refs:  []publish.SymbolicID{op.Node},
				},
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
// sorted GroupKey order for determinism.
func pack(units []taggedUnit, cm backend.ConstraintModel) []publish.PackedTxn {
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
		txns = append(txns, packGroup(g, gu, cm)...)
	}
	return txns
}

// packGroup next-fit packs one group's ordered units: a single open bin accepts
// units until it refuses, then it is sealed and a fresh bin opened. Each sealed
// bin becomes a PackedTxn.
func packGroup(g publish.GroupKey, gu []taggedUnit, cm backend.ConstraintModel) []publish.PackedTxn {
	var txns []publish.PackedTxn
	bin := cm.NewBin()
	acc := newAccumulator(g)
	filled := false // whether the open bin holds at least one unit

	for _, tu := range gu {
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
}

func newAccumulator(g publish.GroupKey) *accumulator {
	return &accumulator{group: g}
}

func (a *accumulator) add(tu taggedUnit) {
	a.refs = append(a.refs, tu.unit.Refs...)
	a.anchors = append(a.anchors, tu.unit.Anchors...)
	a.produces = append(a.produces, tu.produces...)
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
		if name, ok := publish.AnchorRefName(ref); ok && hosted[name] {
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
			if name, ok := publish.AnchorRefName(ref); ok {
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
