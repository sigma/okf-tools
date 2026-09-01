// Package transport is Stage 3 of the okfpub pipeline: it drains the
// transaction-DAG wavefront-by-wavefront against a backend Executor.
//
// Transport owns the resolution table (symbolic id → real backend id): it seeds
// the table from the scan's CurrentState, gates each PackedTxn on all its Refs
// being resolved, calls Execute (behind which the backend performs the physical
// ref substitution), and merges each ExecResult back so downstream transactions
// resolve.
//
// Rate limiting is deliberately NOT here. A backend's rate limit is measured in
// requests, and one transaction is not one request (a create fans out into a
// POST, an anchor re-materialization, a children read), while some backend
// traffic — the scan, write-back's property writes — is not a transaction at all.
// Pacing and retry therefore live at the backend's own request chokepoint, where
// every call passes exactly once (sigma/okf-tools#129); the Notion backend's is
// notion.(*Backend).do.
//
// Concurrency and partial-batch resumability are deferred (see sigma/ideas#172
// "Out of Scope"). This drain is therefore sequential and executes a wavefront's
// transactions in deterministic index order, which keeps the recorded
// transaction stream reproducible; the resolution table's per-lookup mutex is a
// foundation a future concurrent drain can build on, not a claim that this drain
// is concurrent.
//
// See sigma/ideas#172 (ratified #162, #163).
package transport

import (
	"context"
	"fmt"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/optimize"
)

// Transport drains transaction-DAGs against one Executor. Construct it with New;
// the zero value is not usable.
type Transport struct {
	exec backend.Executor
}

// New builds a Transport over exec.
func New(exec backend.Executor) *Transport {
	return &Transport{exec: exec}
}

// Result is the outcome of a drained publish: the resolution-table updates
// execution produced this run — the new symbolic-id → BackendID pairs for created
// nodes and the anchor-name → BackendID pairs for hosted anchors. Scan-seeded
// entries are not repeated here (they were already resolved before the run);
// together with the seed they form the full final resolution table.
type Result struct {
	// Nodes maps each symbolic id created this run to its minted backend id.
	Nodes map[publish.SymbolicID]publish.BackendID
	// Anchors maps each anchor hosted this run to its minted backend id.
	Anchors map[publish.AnchorName]publish.BackendID
}

// Run drains dag wavefront-by-wavefront, seeding the resolution table from seed
// (nil is treated as an empty snapshot) and executing each PackedTxn exactly once,
// only after every one of its Refs resolves. It returns the resolution-table
// updates the run produced.
//
// A wavefront is the set of not-yet-executed transactions whose Refs all resolve
// against the current table AND whose group has no earlier transaction still
// pending; its members are executed in ascending index order, each result merged
// back before the next wavefront is computed. If a wavefront comes up empty while
// transactions remain, Run fails rather than spinning.
//
// The in-order rule within a Group is a correctness constraint the Ref edges do not
// supply. The optimizer packs one node's content as an ordered SEQUENCE of
// transactions, but a continuation chunk's only Ref is the node's own id, so on a
// re-publish (where that id is scan-seeded, produced by nothing this run) no edge
// orders the chunks at all. Left to Refs alone, a chunk whose content links a page
// created this run waits while a later chunk of the same page runs ahead of it —
// which lands the page's body out of order, and, since the first chunk is the one
// that asserts (replaces) the node's content, silently destroys the chunks that
// jumped the queue (sigma/okf-tools#130). Holding a group to its packing order costs
// nothing when nothing is blocked and cannot deadlock an acyclic DAG: a group waits
// only on its own earlier transaction, which is itself waiting on a producer
// elsewhere.
func (t *Transport) Run(ctx context.Context, dag *optimize.TxnDAG, seed *publish.CurrentState) (*Result, error) {
	tbl := newTable(seed)

	// remaining holds the indices still to execute, kept in ascending order so the
	// executed stream is deterministic.
	remaining := make([]int, len(dag.Txns))
	for i := range remaining {
		remaining[i] = i
	}

	// Write-back is incremental, per group (sigma/okf-tools#135). byGroup holds each
	// group's transaction indices and done counts how many of them have executed;
	// when a group's count reaches its size, that group's provenance is persisted
	// immediately, rather than the whole run's being held until the last transaction
	// of the last group lands. An interrupted run then
	// leaves the mirror describing what it actually completed, instead of describing
	// a state that predates every write it made.
	//
	// The unit is the GROUP, not the transaction: the optimizer packs one node's
	// content as an ordered sequence, and only once all of it has landed does the
	// node's recorded hash describe the body the mirror holds. Recording after the
	// first chunk would claim a complete body for a half-written page, and the next
	// run would hash-skip it — worse than recording nothing.
	byGroup := map[publish.GroupKey][]int{}
	for i, txn := range dag.Txns {
		byGroup[txn.Group] = append(byGroup[txn.Group], i)
	}
	done := map[publish.GroupKey]int{}

	for len(remaining) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		var ready, blocked []int
		// stalled holds the groups with an earlier still-unexecuted transaction, so a
		// later one of the same group waits behind it (see the in-order rule above).
		stalled := map[publish.GroupKey]bool{}
		for _, i := range remaining {
			g := dag.Txns[i].Group
			switch {
			case stalled[g], !tbl.resolves(dag.Txns[i].Refs):
				stalled[g] = true
				blocked = append(blocked, i)
			default:
				ready = append(ready, i)
			}
		}
		if len(ready) == 0 {
			return nil, fmt.Errorf("transport: %d transaction(s) cannot proceed; first blocked txn %d (group %s) needs %v",
				len(blocked), blocked[0], dag.Txns[blocked[0]].Group, tbl.unresolved(dag.Txns[blocked[0]].Refs))
		}

		for _, i := range ready {
			txn := dag.Txns[i]
			res, err := t.exec.Execute(ctx, txn.Txn, tbl)
			if err != nil {
				return nil, fmt.Errorf("transport: execute txn %d (group %s): %w", i, txn.Group, err)
			}
			tbl.merge(res)

			done[txn.Group]++
			if done[txn.Group] < len(byGroup[txn.Group]) {
				continue
			}
			if err := t.writeBack(ctx, dag, tbl, byGroup[txn.Group]); err != nil {
				return nil, err
			}
		}
		remaining = blocked
	}

	return &Result{Nodes: tbl.nodesCopy(), Anchors: tbl.anchorsCopy()}, nil
}

// writeBack persists one completed group's provenance (#167 decision 7, made
// incremental by #135): it assembles the record from that group's transactions and
// the resolution table as it stands, and hands it to the backend. It is an
// obligation of the execution path, so the transport owns triggering it; the
// backend that knows its derived-column shape performs the actual writes.
//
// A backend that does not implement WriteBacker, and a group that wrote no node
// content (a DeleteNode archive, whose provenance is empty), write nothing.
func (t *Transport) writeBack(ctx context.Context, dag *optimize.TxnDAG, tbl *table, idxs []int) error {
	wb, ok := t.exec.(backend.WriteBacker)
	if !ok {
		return nil
	}
	prov := buildProvenance(dag, tbl, idxs)
	if len(prov.Nodes) == 0 {
		return nil
	}
	if err := wb.WriteBack(ctx, prov); err != nil {
		return fmt.Errorf("transport: write-back: %w", err)
	}
	return nil
}

// buildProvenance assembles the write-back provenance for the transactions named by
// idxs — one completed group — from the resolution table as it stands. Each
// transaction that wrote a node's content (a non-empty Hash — this excludes
// DeleteNode archives) contributes that node's resolved id, expected hash, parent
// routing, and any hosted anchors. Transactions of the same node (a fused create
// plus its content overflow) merge into one record: the hash/parent agree, and the
// anchor maps union.
func buildProvenance(dag *optimize.TxnDAG, tbl *table, idxs []int) publish.Provenance {
	prov := publish.Provenance{Nodes: map[publish.SymbolicID]publish.NodeProvenance{}}
	for _, i := range idxs {
		txn := dag.Txns[i]
		if txn.Hash == "" {
			continue // no node content written (e.g. a DeleteNode) — nothing to record
		}
		node := publish.SymbolicID(txn.Group)
		id, ok := tbl.Resolve(node)
		if !ok {
			continue // unresolved write-target; the drain would have failed already
		}

		np, seen := prov.Nodes[node]
		if !seen {
			np = publish.NodeProvenance{ID: id, NodeStamp: txn.NodeStamp}
			// Resolve the OWNING ROW — where this node's record goes — which is not the
			// parent: the parent is where the page lives, and the two differ as soon as
			// the mirror nests more than one level (#141).
			if txn.Owner != "" {
				if oid, ok := tbl.Resolve(txn.Owner); ok {
					np.OwnerID = oid
				}
			}
		}
		for _, name := range txn.Anchors {
			aid, ok := tbl.Resolve(publish.AnchorRef(name))
			if !ok {
				continue
			}
			if np.Anchors == nil {
				np.Anchors = map[publish.AnchorName]publish.BackendID{}
			}
			np.Anchors[name] = aid
		}
		prov.Nodes[node] = np
	}
	return prov
}
