package request

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestBackoffGrowsAndIsBounded: the delay doubles per attempt and never exceeds
// MaxBackoff, so a long retry streak parks the run for a bounded time rather than
// an exponentially growing one.
func TestBackoffGrowsAndIsBounded(t *testing.T) {
	for attempt := 1; attempt <= 10; attempt++ {
		d := Backoff(attempt)
		if d <= 0 {
			t.Errorf("attempt %d: delay = %v, want a positive delay", attempt, d)
		}
		if d > MaxBackoff {
			t.Errorf("attempt %d: delay = %v, want at most %v", attempt, d, MaxBackoff)
		}
	}
	// Full jitter means the delay is uniform over (0, ceiling], so the first
	// attempt can never exceed the base.
	for i := 0; i < 50; i++ {
		if d := Backoff(1); d > BaseBackoff {
			t.Fatalf("first attempt delay = %v, want at most %v", d, BaseBackoff)
		}
	}
}

// TestBackoffToleratesAZerothAttempt: callers number attempts differently, and a
// shift of one must not produce a zero or negative delay.
func TestBackoffToleratesAZerothAttempt(t *testing.T) {
	if d := Backoff(0); d <= 0 || d > BaseBackoff {
		t.Errorf("Backoff(0) = %v, want a positive delay within the base ceiling", d)
	}
}

// TestRetryAfterSeconds is the common form: a server naming a delay in seconds is
// obeyed, and clamped so it cannot park the run indefinitely.
func TestRetryAfterSeconds(t *testing.T) {
	h := http.Header{"Retry-After": []string{"3"}}
	d, ok := RetryAfter(h, nil)
	if !ok || d != 3*time.Second {
		t.Errorf("RetryAfter(3) = %v,%v, want 3s,true", d, ok)
	}

	h = http.Header{"Retry-After": []string{"99999"}}
	if d, ok := RetryAfter(h, nil); !ok || d != MaxRetryAfter {
		t.Errorf("RetryAfter(99999) = %v,%v, want it clamped to %v", d, ok, MaxRetryAfter)
	}
}

// TestRetryAfterHTTPDate covers the second form, against an injected clock so the
// assertion does not depend on the wall clock.
func TestRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	h := http.Header{"Retry-After": []string{now.Add(5 * time.Second).Format(http.TimeFormat)}}
	d, ok := RetryAfter(h, clock)
	if !ok || d != 5*time.Second {
		t.Errorf("RetryAfter(date +5s) = %v,%v, want 5s,true", d, ok)
	}

	// An already-elapsed date asks for nothing, so the caller backs off instead.
	past := http.Header{"Retry-After": []string{now.Add(-time.Minute).Format(http.TimeFormat)}}
	if _, ok := RetryAfter(past, clock); ok {
		t.Error("an elapsed Retry-After must not be honoured")
	}
}

// TestRetryAfterRejectsUnusable: absent, unparseable and non-positive values all
// report false so the caller falls back to its own backoff.
func TestRetryAfterRejectsUnusable(t *testing.T) {
	for _, v := range []string{"", "soon", "0", "-5"} {
		h := http.Header{}
		if v != "" {
			h.Set("Retry-After", v)
		}
		if d, ok := RetryAfter(h, nil); ok {
			t.Errorf("Retry-After %q = %v,true, want it rejected", v, d)
		}
	}
}

// TestDelayPrefersRetryAfter: a server that names a delay overrules the client's
// own backoff. This is the behaviour the Docs client did not have.
func TestDelayPrefersRetryAfter(t *testing.T) {
	h := http.Header{"Retry-After": []string{"7"}}
	if d := Delay(h, 1, nil); d != 7*time.Second {
		t.Errorf("Delay with Retry-After = %v, want the server's 7s", d)
	}
	if d := Delay(http.Header{}, 1, nil); d <= 0 || d > BaseBackoff {
		t.Errorf("Delay without Retry-After = %v, want the jittered backoff", d)
	}
}

// TestStalledDistinguishesTimeoutFromCancellation is the distinction that decides
// whether a retry is even considered: a cancelled run stops, it does not back off
// and try again.
func TestStalledDistinguishesTimeoutFromCancellation(t *testing.T) {
	caller := context.Background()
	if !Stalled(caller, context.DeadlineExceeded) {
		t.Error("a deadline firing under a live caller is this client's own stall")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if Stalled(cancelled, context.DeadlineExceeded) {
		t.Error("a deadline under a cancelled caller is the caller giving up, not a stall")
	}
	if Stalled(caller, errors.New("connection reset")) {
		t.Error("only a deadline is a stall")
	}
}

// TestWrapErrNamesTheTimeoutBudget: a stall must read as "the destination stopped
// answering", not as a bare context error that looks like a bug in this program.
func TestWrapErrNamesTheTimeoutBudget(t *testing.T) {
	err := WrapErr(context.Background(), "gdocs", "POST", "/v1/documents", 30*time.Second, context.DeadlineExceeded)
	for _, want := range []string{"gdocs", "POST", "/v1/documents", "no response within 30s", "request timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err, want)
		}
	}

	plain := WrapErr(context.Background(), "notion", "GET", "/v1/pages", time.Second, errors.New("connection reset"))
	if strings.Contains(plain.Error(), "request timeout") {
		t.Errorf("a non-timeout must not be reported as one: %q", plain)
	}
}

// TestWithTimeoutAlwaysReturnsACancel: the caller releases the context on every
// path, deadline or not, so a non-positive budget still cleans up.
func TestWithTimeoutAlwaysReturnsACancel(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), 0)
	if cancel == nil {
		t.Fatal("cancel must never be nil")
	}
	if _, ok := ctx.Deadline(); ok {
		t.Error("a non-positive budget must set no deadline")
	}
	cancel()

	ctx, cancel = WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Error("a positive budget must set a deadline")
	}
}
