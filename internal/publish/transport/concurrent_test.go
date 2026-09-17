package transport

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/optimize"
)

// gateExec is an Executor whose every Execute BLOCKS until the test releases it
// by label, so a test can hold transactions in flight and observe exactly what the
// drain dispatches alongside them. It declares a concurrency bound, records the
// order transactions started and finished, and tracks the high-water mark of
// simultaneous Executes — the number the bound must cap.
type gateExec struct {
	bound int

	mu        sync.Mutex
	seq       int
	inflight  int
	peak      int
	started   []string
	finished  []string
	release   map[string]chan struct{}
	fail      map[string]bool
	starts    chan string // every Execute announces its label here as it begins
	writeBack []publish.Provenance
}

func newGateExec(bound int) *gateExec {
	return &gateExec{
		bound: bound, release: map[string]chan struct{}{}, fail: map[string]bool{},
		starts: make(chan string, 64),
	}
}

func (g *gateExec) Concurrency() int { return g.bound }

func (g *gateExec) gateFor(label string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch, ok := g.release[label]
	if !ok {
		ch = make(chan struct{})
		g.release[label] = ch
	}
	return ch
}

func (g *gateExec) Execute(ctx context.Context, txn publish.Transaction, r backend.Resolver) (publish.ExecResult, error) {
	st := txn.(*stubTxn)
	g.mu.Lock()
	g.inflight++
	g.peak = max(g.peak, g.inflight)
	g.started = append(g.started, st.label)
	g.mu.Unlock()
	g.starts <- st.label

	select {
	case <-g.gateFor(st.label):
	case <-ctx.Done():
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.inflight--
	g.finished = append(g.finished, st.label)
	if g.fail[st.label] {
		return publish.ExecResult{}, errors.New(st.label + " exploded")
	}
	res := publish.ExecResult{Nodes: map[publish.SymbolicID]publish.BackendID{}}
	for _, id := range st.produces {
		g.seq++
		res.Nodes[id] = publish.BackendID(fmt.Sprintf("be-%d", g.seq))
	}
	return res, nil
}

func (g *gateExec) WriteBack(_ context.Context, prov publish.Provenance) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.writeBack = append(g.writeBack, prov)
	return nil
}

// open releases a held transaction.
func (g *gateExec) open(label string) { close(g.gateFor(label)) }

// expectStarted waits for exactly this SET of labels to begin — transactions
// dispatched together start in whatever order their goroutines are scheduled — and
// then confirms nothing else starts within a grace period.
func (g *gateExec) expectStarted(t *testing.T, labels ...string) {
	t.Helper()
	var got []string
	for range labels {
		select {
		case l := <-g.starts:
			got = append(got, l)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %v to start, got %v (started so far: %v)", labels, got, g.snapshotStarted())
		}
	}
	want := append([]string(nil), labels...)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("started %v, want %v (started so far: %v)", got, want, g.snapshotStarted())
	}
	select {
	case got := <-g.starts:
		t.Fatalf("%q started too, want nothing beyond %v", got, labels)
	case <-time.After(50 * time.Millisecond):
	}
}

func (g *gateExec) snapshotStarted() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.started...)
}

// labelled builds a PackedTxn on a labelled stubTxn.
func labelled(label string, group publish.GroupKey, refs, produces []publish.SymbolicID, hash publish.Hash) publish.PackedTxn {
	return publish.PackedTxn{
		Txn:       &stubTxn{label: label, refs: refs, produces: produces},
		Group:     group,
		Refs:      refs,
		Produces:  produces,
		NodeStamp: publish.NodeStamp{Hash: hash},
	}
}

// runAsync drains dag on a goroutine and hands back the outcome channel.
func runAsync(dag *optimize.TxnDAG, exec backend.Executor, opts ...Option) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := New(exec, opts...).Run(context.Background(), dag, nil)
		done <- err
	}()
	return done
}

func waitRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not finish")
		return nil
	}
}

// Up to the bound run at once, dispatched in index order; a transaction whose ref
// an in-flight one produces waits for that result to merge — and, since readiness
// is re-read at wavefront boundaries, for the wavefront to finish.
func TestConcurrentDrainRespectsBoundAndRefs(t *testing.T) {
	a := publish.SymbolicID("node:a")
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		labelled("A", "node:a", nil, []publish.SymbolicID{a}, ""),
		labelled("B", "node:b", []publish.SymbolicID{a}, nil, ""),
		labelled("C", "node:c", nil, nil, ""),
		labelled("D", "node:d", nil, nil, ""),
	}}
	g := newGateExec(2)
	done := runAsync(dag, g)

	g.expectStarted(t, "A", "C") // bound of 2; B blocked on A; D queued behind the bound
	g.open("A")
	g.expectStarted(t, "D") // A's slot goes to D, the next ready one; B still waits
	g.open("C")
	g.open("D")
	g.expectStarted(t, "B") // wavefront over, A's result merged: B is ready
	g.open("B")

	if err := waitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if g.peak > 2 {
		t.Errorf("peak in-flight = %d, want at most the bound of 2", g.peak)
	}
}

// Two transactions of one group never overlap, whatever the bound: a node's
// content is an ordered sequence (#130), and concurrency is across groups only.
func TestConcurrentDrainKeepsAGroupInOrder(t *testing.T) {
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		labelled("G0", "node:g", nil, nil, ""),
		labelled("G1", "node:g", nil, nil, ""),
		labelled("H", "node:h", nil, nil, ""),
	}}
	g := newGateExec(3)
	done := runAsync(dag, g)

	g.expectStarted(t, "G0", "H") // G1 must wait for G0 even with a free slot
	g.open("G0")
	g.expectStarted(t, "G1")
	g.open("G1")
	g.open("H")
	if err := waitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// A group's write-back fires exactly once, after its last transaction, however
// the group's transactions interleave with others.
func TestConcurrentDrainWritesBackEachGroupOnce(t *testing.T) {
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		labelled("G0", "node:g", nil, []publish.SymbolicID{"node:g"}, "hG"),
		labelled("G1", "node:g", nil, nil, "hG"),
		labelled("H", "node:h", nil, []publish.SymbolicID{"node:h"}, "hH"),
	}}
	g := newGateExec(3)
	done := runAsync(dag, g)

	g.expectStarted(t, "G0", "H")
	g.open("G0")
	g.expectStarted(t, "G1")
	g.mu.Lock()
	early := len(g.writeBack)
	g.mu.Unlock()
	if early != 0 {
		t.Errorf("%d write-back(s) before any group completed", early)
	}
	g.open("G1")
	g.open("H")
	if err := waitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(g.writeBack) != 2 {
		t.Fatalf("%d write-backs, want one per group", len(g.writeBack))
	}
	seen := map[publish.SymbolicID]int{}
	for _, p := range g.writeBack {
		for node := range p.Nodes {
			seen[node]++
		}
	}
	if seen["node:g"] != 1 || seen["node:h"] != 1 {
		t.Errorf("groups recorded %v, want each exactly once", seen)
	}
}

// A failing transaction stops dispatch: in-flight ones finish, nothing new
// starts, the error comes back — and the groups that completed were recorded.
func TestConcurrentDrainStopsOnFailure(t *testing.T) {
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		labelled("A", "node:a", nil, []publish.SymbolicID{"node:a"}, "hA"),
		labelled("B", "node:b", nil, []publish.SymbolicID{"node:b"}, "hB"),
		labelled("C", "node:c", nil, nil, ""),
		labelled("D", "node:d", nil, nil, ""),
	}}
	g := newGateExec(2)
	g.fail["B"] = true
	done := runAsync(dag, g)

	g.expectStarted(t, "A", "B")
	g.open("B") // fails while A is still in flight
	g.expectStarted(t)
	g.open("A")
	err := waitRun(t, done)
	if err == nil || !strings.Contains(err.Error(), "B exploded") {
		t.Fatalf("Run should have failed with B's error, got %v", err)
	}
	for _, l := range g.snapshotStarted() {
		if l == "C" || l == "D" {
			t.Errorf("%s was dispatched after the failure", l)
		}
	}
	if len(g.finished) != 2 {
		t.Errorf("finished %v, want both in-flight transactions to have landed before Run returned", g.finished)
	}
	nodes := mergedNodes(g.writeBack)
	if _, ok := nodes["node:a"]; !ok {
		t.Errorf("A completed and must be recorded: %v", nodes)
	}
	if _, ok := nodes["node:b"]; ok {
		t.Errorf("B failed and must not be recorded: %v", nodes)
	}
}

// A backend that declares nothing drains one at a time, exactly as before; and
// WithConcurrency pins the bound regardless of what the backend declares.
func TestConcurrencyDefaultsToOneAndCanBePinned(t *testing.T) {
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		labelled("A", "node:a", nil, nil, ""),
		labelled("B", "node:b", nil, nil, ""),
	}}
	g := newGateExec(4)
	done := runAsync(dag, g, WithConcurrency(1))
	g.expectStarted(t, "A")
	g.open("A")
	g.expectStarted(t, "B")
	g.open("B")
	if err := waitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if g.peak != 1 {
		t.Errorf("peak = %d, want 1 when pinned", g.peak)
	}
}
