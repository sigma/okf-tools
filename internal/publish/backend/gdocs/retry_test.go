package gdocs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newRetryClient builds a client pointed at srv whose retry pause is recorded
// rather than waited out, so these tests run at full speed. Before the sleep
// seam existed this loop could only pause on the wall clock, which is why it
// had no tests at all.
func newRetryClient(srv *httptest.Server, delays *[]time.Duration) *client {
	return &client{
		http:  srv.Client(),
		docs:  srv.URL,
		drive: srv.URL,
		now:   func() time.Time { return time.Unix(0, 0) },
		sleep: func(ctx context.Context, d time.Duration) error {
			*delays = append(*delays, d)
			return ctx.Err()
		},
	}
}

// TestRetriesThrottledThenSucceeds pins the retry loop's happy path: a 429 is
// re-sent rather than surfaced, and the run's stats count both the throttle and
// every attempt.
func TestRetriesThrottledThenSucceeds(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"documentId":"doc-1"}`))
	}))
	defer srv.Close()

	var delays []time.Duration
	c := newRetryClient(srv, &delays)
	var out struct {
		DocumentID string `json:"documentId"`
	}
	if err := c.do(context.Background(), http.MethodGet, srv.URL+"/v1/documents/doc-1", nil, &out); err != nil {
		t.Fatalf("do: %v", err)
	}
	if out.DocumentID != "doc-1" {
		t.Errorf("documentId = %q, want doc-1", out.DocumentID)
	}
	if n != 2 {
		t.Errorf("server saw %d attempts, want 2", n)
	}
	// The server asked for 2s; honouring Retry-After is the behaviour the shared
	// policy added and this client used to ignore.
	if len(delays) != 1 || delays[0] != 2*time.Second {
		t.Errorf("delays = %v, want one 2s pause", delays)
	}
	if st := c.RequestStats(); st.Requests != 2 || st.Throttled != 1 {
		t.Errorf("stats = %+v, want 2 requests / 1 throttled", st)
	}
}

// TestRetriesGiveUpBounded pins the bound: a server that fails forever costs a
// bounded amount of backoff and then fails, rather than retrying without end.
// The bound is TIME, at least RetryBudget, so it outlasts a per-minute quota
// window (#218).
func TestRetriesGiveUpBounded(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var delays []time.Duration
	c := newRetryClient(srv, &delays)
	err := c.do(context.Background(), http.MethodGet, srv.URL+"/v1/documents/doc-1", nil, nil)
	if err == nil {
		t.Fatal("a permanently failing server should fail the call")
	}
	if n != len(delays)+1 {
		t.Errorf("server saw %d attempts for %d pauses, want one more attempt than pauses", n, len(delays))
	}
	var total time.Duration
	for i, d := range delays {
		// No Retry-After: every pause falls back to the jittered exponential
		// backoff, bounded by the shared policy's ceiling.
		if d <= 0 || d > 8*time.Second {
			t.Errorf("delay %d = %v, outside (0, 8s]", i, d)
		}
		total += d
	}
	if total < RetryBudget || total > RetryBudget+8*time.Second {
		t.Errorf("paused %v in total, want at least the %v budget and at most one pause over it", total, RetryBudget)
	}
	if st := c.RequestStats(); st.Transient != n {
		t.Errorf("stats = %+v; every attempt of a request that ended in failure must count", st)
	}
}

// TestThrottlingOutlastsAQuotaWindow is #218's first failure: Docs answered 429
// for longer than five attempts lasted, and the run gave up inside the
// per-minute window. A burst covering 65s of backoff must now be ridden out.
func TestThrottlingOutlastsAQuotaWindow(t *testing.T) {
	var delays []time.Duration
	var waited time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if waited < 65*time.Second {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newRetryClient(srv, &delays)
	c.sleep = func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		waited += d
		return ctx.Err()
	}
	if err := c.do(context.Background(), http.MethodPost, srv.URL+"/v1/documents/doc-1:batchUpdate", map[string]any{}, nil); err != nil {
		t.Fatalf("a 65s throttling burst was not ridden out: %v", err)
	}
	if st := c.RequestStats(); st.Throttled != len(delays) || st.Throttled < 5 {
		t.Errorf("stats = %+v after %d pauses; want every throttled attempt counted", st, len(delays))
	}
}

// TestDriveRateLimit403IsRetried is #218's second failure: Drive reports its
// rate limit as a 403 with the reason in the body, and the sidecar write failed
// on it. Any other 403 must stay terminal.
func TestDriveRateLimit403IsRetried(t *testing.T) {
	const limited = `{"error":{"code":403,"message":"User rate limit exceeded.",` +
		`"errors":[{"domain":"usageLimits","reason":"userRateLimitExceeded","message":"User rate limit exceeded."}]}}`
	const denied = `{"error":{"code":403,"message":"The user does not have sufficient permissions for this file.",` +
		`"errors":[{"domain":"global","reason":"insufficientFilePermissions"}]}}`

	for _, tc := range []struct {
		name  string
		body  string
		retry bool
	}{{"userRateLimitExceeded", limited, true}, {"insufficientFilePermissions", denied, false}} {
		t.Run(tc.name, func(t *testing.T) {
			var n int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n++
				if n == 1 {
					w.WriteHeader(http.StatusForbidden)
					w.Write([]byte(tc.body))
					return
				}
				w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			var delays []time.Duration
			c := newRetryClient(srv, &delays)
			err := c.do(context.Background(), http.MethodPatch, srv.URL+"/upload/drive/v3/files/state", map[string]any{}, nil)
			if tc.retry && (err != nil || n != 2 || c.RequestStats().Throttled != 1) {
				t.Errorf("a rate-limit 403 must be retried and counted: err=%v, attempts=%d, stats=%+v", err, n, c.RequestStats())
			}
			if !tc.retry && (err == nil || n != 1) {
				t.Errorf("a permission 403 must fail at once: err=%v, attempts=%d", err, n)
			}
		})
	}
}

// TestRetryStopsOnCancelledRun proves a cancelled run is not held hostage by the
// backoff: the pause reports the cancellation and the loop gives up there.
func TestRetryStopsOnCancelledRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := &client{
		http: srv.Client(), docs: srv.URL, drive: srv.URL,
		sleep: func(context.Context, time.Duration) error { cancel(); return context.Canceled },
	}
	if err := c.do(ctx, http.MethodGet, srv.URL+"/v1/documents/doc-1", nil, nil); err == nil {
		t.Fatal("a cancelled run should fail rather than keep backing off")
	}
}
