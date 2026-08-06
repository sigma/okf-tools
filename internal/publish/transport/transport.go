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
// against the current table; its members are executed in ascending index order,
// each result merged back before the next wavefront is computed. If a wavefront
// comes up empty while transactions remain, their Refs are unsatisfiable and Run
// fails rather than spinning.
func (t *Transport) Run(ctx context.Context, dag *optimize.TxnDAG, seed *publish.CurrentState) (*Result, error) {
	tbl := newTable(seed)

	// remaining holds the indices still to execute, kept in ascending order so the
	// executed stream is deterministic.
	remaining := make([]int, len(dag.Txns))
	for i := range remaining {
		remaining[i] = i
	}

	for len(remaining) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		var ready, blocked []int
		for _, i := range remaining {
			if tbl.resolves(dag.Txns[i].Refs) {
				ready = append(ready, i)
			} else {
				blocked = append(blocked, i)
			}
		}
		if len(ready) == 0 {
			return nil, fmt.Errorf("transport: %d transaction(s) have unresolvable refs; first blocked txn %d needs %v",
				len(blocked), blocked[0], tbl.unresolved(dag.Txns[blocked[0]].Refs))
		}

		for _, i := range ready {
			txn := dag.Txns[i]
			res, err := t.exec.Execute(ctx, txn.Txn, tbl)
			if err != nil {
				return nil, fmt.Errorf("transport: execute txn %d (group %s): %w", i, txn.Group, err)
			}
			tbl.merge(res)
		}
		remaining = blocked
	}

	// Publish-time write-back (#167 decision 7): once the whole graph has drained,
	// persist the run's provenance back into the backend's self-describing state so
	// the next ScanStored is accurate. It is an obligation of the execution path, so
	// the transport owns triggering it; the backend that knows its derived-column
	// shape performs the actual writes. A backend that does not implement WriteBacker
	// (or an unchanged run, whose provenance is empty) writes nothing.
	if wb, ok := t.exec.(backend.WriteBacker); ok {
		prov := buildProvenance(dag, tbl)
		if len(prov.Nodes) > 0 {
			if err := wb.WriteBack(ctx, prov); err != nil {
				return nil, fmt.Errorf("transport: write-back: %w", err)
			}
		}
	}

	return &Result{Nodes: tbl.nodesCopy(), Anchors: tbl.anchorsCopy()}, nil
}

// buildProvenance assembles the run's write-back provenance from the drained
// transaction-DAG and the final resolution table. Each transaction that wrote a
// node's content (a non-empty Hash — this excludes DeleteNode archives) contributes
// that node's resolved id, expected hash, parent routing, and any hosted anchors.
// Transactions of the same node (a fused create plus its content overflow) merge
// into one record: the hash/parent agree, and the anchor maps union.
func buildProvenance(dag *optimize.TxnDAG, tbl *table) publish.Provenance {
	prov := publish.Provenance{Nodes: map[publish.SymbolicID]publish.NodeProvenance{}}
	for _, txn := range dag.Txns {
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
			if txn.Parent != "" {
				if pid, ok := tbl.Resolve(txn.Parent); ok {
					np.ParentID = pid
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
