package transport

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/optimize"
)

// --- a controllable stub Executor -------------------------------------------

// stubTxn is a transparent stand-in for the opaque backend Transaction so a test
// can assert both what a transaction produces and that its refs were resolved by
// the time the transport executed it.
type stubTxn struct {
	refs     []publish.SymbolicID
	produces []publish.SymbolicID
	anchors  []publish.AnchorName
}

// execCall records one Execute invocation: the transaction's group ordinal and,
// crucially, whether every declared ref resolved through the Resolver the
// transport passed in — the observable proof that readiness gating held.
type execCall struct {
	produces    []publish.SymbolicID
	allResolved bool
}

// stubExec mints deterministic backend ids and logs each call in execution order.
type stubExec struct {
	mu   sync.Mutex
	seq  int
	log  []execCall
	fail bool // when true, Execute returns an error
}

func (e *stubExec) Execute(_ context.Context, txn publish.Transaction, r backend.Resolver) (publish.ExecResult, error) {
	st := txn.(*stubTxn)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fail {
		return publish.ExecResult{}, fmt.Errorf("boom")
	}

	allResolved := true
	for _, ref := range st.refs {
		if _, ok := r.Resolve(ref); !ok {
			allResolved = false
		}
	}

	res := publish.ExecResult{
		Nodes:   map[publish.SymbolicID]publish.BackendID{},
		Anchors: map[publish.AnchorName]publish.BackendID{},
	}
	for _, id := range st.produces {
		e.seq++
		res.Nodes[id] = publish.BackendID(fmt.Sprintf("be-%d", e.seq))
	}
	for _, a := range st.anchors {
		e.seq++
		res.Anchors[a] = publish.BackendID(fmt.Sprintf("anc-%d", e.seq))
	}
	e.log = append(e.log, execCall{produces: st.produces, allResolved: allResolved})
	return res, nil
}

// packed wraps a stubTxn as a PackedTxn carrying the metadata the transport gates
// and paces on.
func packed(group publish.GroupKey, refs, produces []publish.SymbolicID, anchors []publish.AnchorName) publish.PackedTxn {
	return publish.PackedTxn{
		Txn:      &stubTxn{refs: refs, produces: produces, anchors: anchors},
		Group:    group,
		Refs:     refs,
		Produces: produces,
		Anchors:  anchors,
	}
}

func run(t *testing.T, dag *optimize.TxnDAG, seed *publish.CurrentState, exec backend.Executor) *Result {
	t.Helper()
	res, err := New(exec, WithInterval(0)).Run(context.Background(), dag, seed)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// --- readiness gating -------------------------------------------------------

// A transaction that refs a node produced by another must execute strictly after
// its producer, and its ref must be resolved by execution time.
func TestReadinessGatesOnRefs(t *testing.T) {
	nodeA := publish.SymbolicID("node:a")
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		// index 0: the consumer — placed first on purpose so a naive in-order drain
		// would execute it before its producer.
		packed("node:consumer", []publish.SymbolicID{nodeA}, []publish.SymbolicID{publish.SymbolicID("node:consumer")}, nil),
		// index 1: the producer of node:a.
		packed("node:a", nil, []publish.SymbolicID{nodeA}, nil),
	}}
	exec := &stubExec{}
	run(t, dag, nil, exec)

	if len(exec.log) != 2 {
		t.Fatalf("want 2 executions, got %d", len(exec.log))
	}
	// Producer of node:a must run first.
	if got := exec.log[0].produces; len(got) != 1 || got[0] != nodeA {
		t.Errorf("first executed txn should produce node:a, produced %v", got)
	}
	// Every executed txn saw all its refs resolved.
	for i, c := range exec.log {
		if !c.allResolved {
			t.Errorf("txn %d executed with an unresolved ref", i)
		}
	}
}

// A ref present only in the scan seed resolves immediately, with no producer this
// run — an existing node or glossary anchor.
func TestScanSeedResolvesRefs(t *testing.T) {
	existing := publish.SymbolicID("node:existing")
	seed := publish.NewCurrentState(
		map[publish.SymbolicID]publish.BackendID{existing: "be-existing"},
		nil,
		map[publish.AnchorName]publish.BackendID{"glossary/term": "be-anchor"},
	)
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		packed("node:p", []publish.SymbolicID{existing, publish.SymbolicID("anchor:glossary/term")},
			[]publish.SymbolicID{publish.SymbolicID("node:p")}, nil),
	}}
	exec := &stubExec{}
	run(t, dag, seed, exec)

	if len(exec.log) != 1 || !exec.log[0].allResolved {
		t.Fatalf("scan-seeded refs should let the txn execute with all refs resolved, log=%+v", exec.log)
	}
}

// The returned Result carries exactly the pairs execution minted this run.
func TestResultCarriesMergedTable(t *testing.T) {
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		packed("node:g", nil, []publish.SymbolicID{publish.SymbolicID("node:g")}, []publish.AnchorName{"glossary/term"}),
	}}
	exec := &stubExec{}
	res := run(t, dag, nil, exec)

	if _, ok := res.Nodes[publish.SymbolicID("node:g")]; !ok {
		t.Errorf("result should record produced node:g, got %v", res.Nodes)
	}
	if _, ok := res.Anchors[publish.AnchorName("glossary/term")]; !ok {
		t.Errorf("result should record hosted anchor, got %v", res.Anchors)
	}
}

// An unsatisfiable ref (no producer, not in the seed) is a hard stop, not a spin.
func TestUnresolvableRefsFail(t *testing.T) {
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		packed("node:p", []publish.SymbolicID{publish.SymbolicID("node:ghost")},
			[]publish.SymbolicID{publish.SymbolicID("node:p")}, nil),
	}}
	_, err := New(&stubExec{}, WithInterval(0)).Run(context.Background(), dag, nil)
	if err == nil {
		t.Fatal("want an error for an unresolvable ref, got nil")
	}
}

// An Executor error aborts the drain and surfaces.
func TestExecuteErrorPropagates(t *testing.T) {
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		packed("node:p", nil, []publish.SymbolicID{publish.SymbolicID("node:p")}, nil),
	}}
	_, err := New(&stubExec{fail: true}, WithInterval(0)).Run(context.Background(), dag, nil)
	if err == nil {
		t.Fatal("want the Executor error to propagate, got nil")
	}
}

// --- pacing -----------------------------------------------------------------

// fakeClock models time advancing only by explicit sleeps, so pacing is exercised
// without any wall-clock delay.
type fakeClock struct {
	t     time.Time
	slept []time.Duration
}

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) doSleep(d time.Duration) { c.slept = append(c.slept, d); c.t = c.t.Add(d) }

// Consecutive transactions of the same Group are spaced by the interval; distinct
// groups are not paced against each other.
func TestPerGroupPacing(t *testing.T) {
	interval := 100 * time.Millisecond
	clock := &fakeClock{t: time.Unix(0, 0)}

	// Three same-group txns (each independent) and one of another group. All refs
	// empty so they all fall in one wavefront and execute in index order.
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		packed("node:g1", nil, []publish.SymbolicID{publish.SymbolicID("node:g1a")}, nil),
		packed("node:g1", nil, []publish.SymbolicID{publish.SymbolicID("node:g1b")}, nil),
		packed("node:g1", nil, []publish.SymbolicID{publish.SymbolicID("node:g1c")}, nil),
		packed("node:g2", nil, []publish.SymbolicID{publish.SymbolicID("node:g2a")}, nil),
	}}
	tr := New(&stubExec{}, WithInterval(interval), withClock(clock.now, clock.doSleep))
	if _, err := tr.Run(context.Background(), dag, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Two intra-group gaps (g1 → g1 → g1) = two sleeps of one interval; g2 is a
	// fresh group so it never sleeps.
	if len(clock.slept) != 2 {
		t.Fatalf("want 2 pacing sleeps, got %d: %v", len(clock.slept), clock.slept)
	}
	for i, d := range clock.slept {
		if d != interval {
			t.Errorf("sleep %d = %v, want %v", i, d, interval)
		}
	}
}

// New defaults the pacing interval to the spec's ~350ms when none is configured.
func TestDefaultInterval(t *testing.T) {
	if got := New(&stubExec{}).interval; got != DefaultInterval {
		t.Errorf("default interval = %v, want %v", got, DefaultInterval)
	}
	if DefaultInterval != 350*time.Millisecond {
		t.Errorf("DefaultInterval = %v, want the spec's ~350ms", DefaultInterval)
	}
}

// Interval 0 disables pacing outright.
func TestZeroIntervalNeverSleeps(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		packed("node:g", nil, []publish.SymbolicID{publish.SymbolicID("node:a")}, nil),
		packed("node:g", nil, []publish.SymbolicID{publish.SymbolicID("node:b")}, nil),
	}}
	tr := New(&stubExec{}, WithInterval(0), withClock(clock.now, clock.doSleep))
	if _, err := tr.Run(context.Background(), dag, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(clock.slept) != 0 {
		t.Errorf("interval 0 should never sleep, slept %v", clock.slept)
	}
}
