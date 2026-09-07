package transport

import (
	"context"
	"fmt"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/optimize"
)

// A drain is otherwise silent between a run's banner and its summary, which is
// how a publish wedged on a stalled request went unnoticed for 34 minutes (#184).
// Progress is reported per transaction, counting up to the DAG's total, so a
// caller can tell a working run from a wedged one.
func TestProgressCountsEveryTransaction(t *testing.T) {
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		packed("a", nil, []publish.SymbolicID{"node:a"}, nil),
		packed("b", []publish.SymbolicID{"node:a"}, []publish.SymbolicID{"node:b"}, nil),
		packed("c", []publish.SymbolicID{"node:b"}, nil, nil),
	}}

	var seen []string
	_, err := New(&stubExec{}, WithProgress(func(done, total int) {
		seen = append(seen, fmt.Sprintf("%d/%d", done, total))
	})).Run(context.Background(), dag, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{"1/3", "2/3", "3/3"}
	if len(seen) != len(want) {
		t.Fatalf("progress = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("progress[%d] = %s, want %s", i, seen[i], want[i])
		}
	}
}

// The counter reports what LANDED, so a run that fails partway stops where it
// stopped rather than claiming the transactions it never reached.
func TestProgressStopsWhereTheRunFailed(t *testing.T) {
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{
		packed("a", nil, nil, nil),
		packed("b", nil, nil, nil),
	}}
	var calls int
	_, err := New(&stubExec{fail: true}, WithProgress(func(int, int) { calls++ })).
		Run(context.Background(), dag, nil)
	if err == nil {
		t.Fatal("Run returned no error")
	}
	if calls != 0 {
		t.Errorf("progress reported %d transaction(s) for a run that executed none", calls)
	}
}

// No reporter is the default: every existing caller drains unchanged.
func TestProgressIsOptional(t *testing.T) {
	dag := &optimize.TxnDAG{Txns: []publish.PackedTxn{packed("a", nil, nil, nil)}}
	if _, err := New(&stubExec{}, WithProgress(nil)).Run(context.Background(), dag, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
