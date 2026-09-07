package notion

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// hangingServer answers nothing: it holds every request open until the client
// gives up, which is the wire behaviour of the half-dead HTTP/2 connection that
// wedged a publish for 34 minutes (#184). The retry loop cannot see it — a
// request that never returns never produces a status to classify.
type hangingServer struct {
	mu    sync.Mutex
	calls int
	// answerFrom, when non-zero, lets the Nth and later requests answer normally, so
	// a test can assert the run RECOVERS rather than merely failing fast.
	answerFrom int
	// stop releases every parked handler at the end of the test. A client that
	// abandons a request does not reliably make the server notice, so without this
	// httptest.Server.Close would block on its own handlers.
	stop chan struct{}
}

func newHangingServer(answerFrom int) *hangingServer {
	return &hangingServer{answerFrom: answerFrom, stop: make(chan struct{})}
}

func (h *hangingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.calls++
	n := h.calls
	h.mu.Unlock()

	if h.answerFrom > 0 && n >= h.answerFrom {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"page-1"}`))
		return
	}
	// Never answer: wait for the client to hang up, or for the test to end.
	select {
	case <-r.Context().Done():
	case <-h.stop:
	}
}

func (h *hangingServer) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func newHangingBackend(t *testing.T, h *hangingServer, opts ...Option) *Backend {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	// Registered second, so it runs FIRST: the parked handlers must be released
	// before Close waits on them.
	t.Cleanup(func() { close(h.stop) })
	base := []Option{
		WithBaseURL(srv.URL), WithToken("tok"), WithDataSourceID("ds1"),
		WithInterval(0), WithLogger(func(string, ...any) {}),
		WithRequestTimeout(50 * time.Millisecond),
	}
	return New(append(base, opts...)...)
}

// TestStalledRequestFailsRatherThanHanging is the #184 regression: with no
// deadline anywhere, this call never returns.
func TestStalledRequestFailsRatherThanHanging(t *testing.T) {
	h := newHangingServer(0)
	be := newHangingBackend(t, h)

	done := make(chan error, 1)
	go func() {
		// POST /pages is NOT replay-safe, so it must fail on the first stall rather
		// than mint a second page on the guess that the first never landed.
		done <- be.do(context.Background(), http.MethodPost, "/pages", map[string]any{}, nil)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stalled request returned no error")
		}
		if !strings.Contains(err.Error(), "timeout") && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error = %v, want it to name the timeout", err)
		}
		if got := h.count(); got != 1 {
			t.Errorf("attempts = %d, want 1: a create that may have landed must not be replayed", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request hung: no deadline bounded it")
	}
}

// A stall on a REPLAY-SAFE route costs one backoff, not the run: replaying a
// PATCH lands the same state whether or not the first attempt reached Notion, so
// the retry loop can absorb the stall the way it absorbs a 429.
func TestStalledReplaySafeRequestIsRetried(t *testing.T) {
	h := newHangingServer(2)
	be := newHangingBackend(t, h)

	var out object
	if err := be.do(context.Background(), http.MethodPatch, "/pages/x", map[string]any{}, &out); err != nil {
		t.Fatalf("do: %v", err)
	}
	if out.ID != "page-1" {
		t.Errorf("id = %q, want page-1", out.ID)
	}
	if got := h.count(); got != 2 {
		t.Errorf("attempts = %d, want 2 (one stalled + one retry)", got)
	}
}

// A stall that never clears is bounded by maxAttempts rather than retried
// forever, and the error says what happened.
func TestStalledRequestGivesUpAfterMaxAttempts(t *testing.T) {
	h := newHangingServer(0)
	be := newHangingBackend(t, h)

	err := be.do(context.Background(), http.MethodPatch, "/pages/x", map[string]any{}, nil)
	if err == nil {
		t.Fatal("a permanently stalled request returned no error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error = %v, want it to name the timeout", err)
	}
	if got := h.count(); got != defaultMaxAttempts {
		t.Errorf("attempts = %d, want %d", got, defaultMaxAttempts)
	}
}

// The caller's own cancellation is not a per-attempt timeout, and must not be
// retried: a cancelled run stops, it does not back off and try again.
func TestCallerCancellationIsNotRetried(t *testing.T) {
	h := newHangingServer(0)
	be := newHangingBackend(t, h, WithRequestTimeout(time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := be.do(ctx, http.MethodPatch, "/pages/x", map[string]any{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to carry context.Canceled", err)
	}
	if got := h.count(); got != 1 {
		t.Errorf("attempts = %d, want 1: a cancelled run must not retry", got)
	}
}

// New wires the default deadline, so a real run is bounded without opting in —
// the wiring nothing else would catch.
func TestDefaultRequestTimeoutIsWired(t *testing.T) {
	if got := New().limits.timeout; got != DefaultRequestTimeout {
		t.Errorf("request timeout = %v, want the default %v", got, DefaultRequestTimeout)
	}
}
