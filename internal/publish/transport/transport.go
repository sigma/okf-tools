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
// The drain executes a wavefront's transactions in deterministic index order, up
// to as many at once as the backend declares it can take (backend.
// ConcurrentExecutor; one, for a backend that declares nothing or a transport
// pinned with WithConcurrency — which keeps the recorded transaction stream
// reproducible, and is what the Google Docs backend relies on for tab placement).
// Readiness is re-read between wavefronts rather than after every landing: the
// barrier costs a few in-flight latencies per wavefront and is what makes the
// one-at-a-time stream identical to what it always was. Partial-batch
// resumability remains deferred (see sigma/ideas#172 "Out of Scope").
//
// See sigma/ideas#172 (ratified #162, #163).
package transport

import (
	"context"
	"fmt"
	"sync"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/optimize"
)

// Transport drains transaction-DAGs against one Executor. Construct it with New;
// the zero value is not usable.
type Transport struct {
	exec backend.Executor
	// progress, when set, is called after each transaction executes with how many
	// of the DAG's transactions have now landed. A drain is otherwise SILENT
	// between the run's banner and its summary, which is how a publish wedged on a
	// stalled request went 34 minutes without anyone being able to tell it apart
	// from a slow one (#184).
	progress func(done, total int)
	// concurrency, when > 0, pins how many transactions may be in flight at once,
	// overriding whatever the backend declares. Zero defers to the backend.
	concurrency int
}

// New builds a Transport over exec.
func New(exec backend.Executor, opts ...Option) *Transport {
	t := &Transport{exec: exec}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Option configures a Transport.
type Option func(*Transport)

// WithProgress reports drain progress: after each transaction executes, done is
// how many have landed of total. A nil function is ignored.
//
// It is called as each transaction lands, under the drain's own lock so the count
// it reports never runs backwards — which means it must not block: whatever it
// does costs the run that time, holds every other landing worker behind it, and a
// reporter that stalls reintroduces exactly the hang it is there to make visible.
func WithProgress(f func(done, total int)) Option {
	return func(t *Transport) {
		if f != nil {
			t.progress = f
		}
	}
}

// WithConcurrency pins how many transactions may be in flight at once, whatever
// the backend declares — the way a test asks for the reproducible one-at-a-time
// stream from a backend that would otherwise run several. Values below 1 pin to 1.
func WithConcurrency(n int) Option {
	return func(t *Transport) { t.concurrency = max(1, n) }
}

// bound reports how many transactions the drain may hold in flight: the pinned
// value if one was given, else what the backend declares, else one.
func (t *Transport) bound() int {
	if t.concurrency > 0 {
		return t.concurrency
	}
	if c, ok := t.exec.(backend.ConcurrentExecutor); ok {
		return max(1, c.Concurrency())
	}
	return 1
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
// pending; its members are dispatched in ascending index order, up to bound() at
// a time, each result merged back as it lands. Readiness is re-read once the
// whole wavefront has landed, not after each result: that is what keeps the
// dispatch order of a one-at-a-time drain identical to what it always was, so a
// backend that declares no concurrency sees the same request stream as before
// (sigma/okf-tools#212). If a wavefront comes up empty while transactions remain,
// Run fails rather than spinning.
//
// The in-order rule within a Group is a correctness constraint the Ref edges do not
// supply. The optimizer packs one node's content as an ordered SEQUENCE of
// transactions, but a continuation chunk's only Ref is the node's own id, so on a
// re-publish (where that id is scan-seeded, produced by nothing this run) no edge
// orders the chunks at all. Left to Refs alone, a chunk whose content links a page
// created this run waits while a later chunk of the same page runs ahead of it —
// which lands the page's body out of order, and, since the first chunk is the one
// that asserts (replaces) the node's content, silently destroys the chunks that
// jumped the queue (sigma/okf-tools#130). Under concurrency the rule gains a
// clause: a group with a transaction IN FLIGHT is held too, so two chunks of one
// node never overlap. Holding a group to its packing order costs nothing when
// nothing is blocked and cannot deadlock an acyclic DAG: a group waits only on its
// own earlier transaction, which is itself waiting on a producer elsewhere.
func (t *Transport) Run(ctx context.Context, dag *optimize.TxnDAG, seed *publish.CurrentState) (*Result, error) {
	var produced []publish.SymbolicID
	for _, txn := range dag.Txns {
		produced = append(produced, txn.Produces...)
	}
	tbl := newTable(seed, produced)

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
	d := &drain{t: t, dag: dag, tbl: tbl, byGroup: byGroup, done: map[publish.GroupKey]int{}, bound: t.bound()}

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

		if err := d.wavefront(ctx, ready); err != nil {
			return nil, err
		}
		remaining = blocked
	}

	return &Result{Nodes: tbl.nodesCopy(), Anchors: tbl.anchorsCopy()}, nil
}

// drain is one Run's dispatch state: the table results merge into, the per-group
// completion counts write-back keys on, and the bound on transactions in flight.
// Its mutex guards done and executed, which workers update as they land.
type drain struct {
	t       *Transport
	dag     *optimize.TxnDAG
	tbl     *table
	byGroup map[publish.GroupKey][]int
	bound   int

	mu       sync.Mutex
	done     map[publish.GroupKey]int
	executed int
}

// landed is what a worker reports back: which transaction finished, and how.
type landed struct {
	idx int
	err error
}

// wavefront dispatches one wavefront's transactions in index order, holding at
// most bound in flight and never two of one group, and returns once every one of
// them has landed. A worker executes its transaction, merges the result, and — if
// that completed the group — writes the group back, all before it reports in; so
// the bound covers write-back's requests too, and a group's next transaction (in a
// later wavefront) never runs ahead of its record.
//
// On the first failure nothing further is dispatched; the transactions already in
// flight are allowed to land (their groups' write-back included, if they complete
// one), and the first error is returned. Completed groups therefore stay recorded
// whatever else the run was doing when it died (#135).
func (d *drain) wavefront(ctx context.Context, ready []int) error {
	pending := ready // not yet dispatched, in index order
	inflight := 0    // dispatched, not yet landed
	busy := map[publish.GroupKey]bool{}
	results := make(chan landed, len(ready))
	var firstErr error

	for len(pending) > 0 || inflight > 0 {
		// Dispatch: the earliest pending transactions whose group is idle, up to the
		// bound. A group's later member stays pending until its earlier one lands.
		if firstErr == nil {
			kept := pending[:0]
			for _, i := range pending {
				g := d.dag.Txns[i].Group
				if inflight < d.bound && !busy[g] {
					busy[g] = true
					inflight++
					go d.execute(ctx, i, results)
					continue
				}
				kept = append(kept, i)
			}
			pending = kept
		} else {
			pending = nil
		}
		if inflight == 0 {
			break
		}

		l := <-results
		inflight--
		busy[d.dag.Txns[l.idx].Group] = false
		if l.err != nil && firstErr == nil {
			firstErr = l.err
		}
	}
	return firstErr
}

// execute runs one transaction to completion on a worker: Execute, merge, count,
// write back if that closed the group, report progress, then report in.
func (d *drain) execute(ctx context.Context, i int, results chan<- landed) {
	txn := d.dag.Txns[i]
	res, err := d.t.exec.Execute(ctx, txn.Txn, d.tbl)
	if err != nil {
		results <- landed{idx: i, err: fmt.Errorf("transport: execute txn %d (group %s): %w", i, txn.Group, err)}
		return
	}
	d.tbl.merge(res)

	d.mu.Lock()
	d.executed++
	d.done[txn.Group]++
	complete := d.done[txn.Group] == len(d.byGroup[txn.Group])
	// Reported under the lock so the count a reporter sees never runs backwards:
	// two workers landing together would otherwise race to report 2 then 1.
	if d.t.progress != nil {
		d.t.progress(d.executed, len(d.dag.Txns))
	}
	d.mu.Unlock()

	if complete {
		err = d.t.writeBack(ctx, d.dag, d.tbl, d.byGroup[txn.Group])
	}
	results <- landed{idx: i, err: err}
}

// writeBack persists one completed group's provenance (#167 decision 7, made
// incremental by #135): it assembles the record from that group's transactions and
// the resolution table as it stands, and hands it to the backend. It is an
// obligation of the execution path, so the transport owns triggering it; the
// backend that knows its derived-column shape performs the actual writes.
//
// A backend that does not implement WriteBacker, and a group that neither wrote a
// node's content nor removed one, write nothing.
func (t *Transport) writeBack(ctx context.Context, dag *optimize.TxnDAG, tbl *table, idxs []int) error {
	wb, ok := t.exec.(backend.WriteBacker)
	if !ok {
		return nil
	}
	prov := buildProvenance(dag, tbl, idxs)
	if len(prov.Nodes) == 0 && len(prov.Deleted) == 0 {
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
//
// A transaction that REMOVED nodes contributes them to Deleted instead — the other
// half of the same record, since a backend has to stop naming a node as
// deliberately as it started (#189). Both halves are assembled here because both
// are read off the executed transactions; only the removals need no resolution,
// their whole content being the symbolic id that is going away.
func buildProvenance(dag *optimize.TxnDAG, tbl *table, idxs []int) publish.Provenance {
	prov := publish.Provenance{Nodes: map[publish.SymbolicID]publish.NodeProvenance{}}
	for _, i := range idxs {
		txn := dag.Txns[i]
		prov.Deleted = append(prov.Deleted, txn.Deletes...)
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
