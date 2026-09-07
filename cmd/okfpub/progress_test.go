package main

import (
	"strings"
	"sync"
	"testing"
)

// syncBuf is a writer the ticker goroutine and the test can both touch.
type syncBuf struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func newTestProgress(label string) (*progress, *syncBuf) {
	buf := &syncBuf{}
	p := newProgress(label)
	p.out = buf
	return p, buf
}

// A run that has executed nothing yet has no transactions to count, and "0/0"
// would claim it does.
func TestProgressSaysNothingBeforeTheDrainStarts(t *testing.T) {
	p, buf := newTestProgress("")
	p.print()
	if got := buf.String(); got != "" {
		t.Errorf("printed %q before any transaction landed", got)
	}
}

// The counter ends at n/n: the last transaction prints itself rather than
// waiting for a tick that the run's end would cancel.
func TestProgressPrintsTheFinalLine(t *testing.T) {
	p, buf := newTestProgress("adr")
	p.report(1, 3)
	p.report(2, 3)
	if got := buf.String(); got != "" {
		t.Errorf("a mid-drain report printed %q; the ticker owns those lines", got)
	}
	p.report(3, 3)
	if got := buf.String(); got != "okfpub: adr: 3/3 transaction(s)\n" {
		t.Errorf("final line = %q", got)
	}
}

// A tick republishes the count as it stands, which is what makes a WEDGED run
// visible: a stuck drain completes nothing, so a count that repeats unchanged is
// the signal.
func TestProgressTickRepublishesTheCount(t *testing.T) {
	p, buf := newTestProgress("")
	p.report(2, 9)
	p.print()
	p.print()
	if got := buf.String(); got != "okfpub: 2/9 transaction(s)\nokfpub: 2/9 transaction(s)\n" {
		t.Errorf("ticks printed %q, want the same count twice", got)
	}
}

// Stopping is synchronous and idempotent: no line may land after the run's
// summary, and a caller that stops twice must not panic on a closed channel.
func TestProgressStopIsSynchronousAndIdempotent(t *testing.T) {
	p, buf := newTestProgress("")
	stop := p.start()
	p.report(1, 2)
	stop()
	stop()
	if got := buf.String(); got != "" {
		t.Errorf("printed %q; a run shorter than the tick interval prints nothing mid-drain", got)
	}
}
