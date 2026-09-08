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

// TestRetriesGiveUpBounded pins the bound: a server that throttles forever costs
// a fixed number of attempts and then fails, rather than retrying without end.
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
	if n != 5 {
		t.Errorf("server saw %d attempts, want 5", n)
	}
	if len(delays) != 4 {
		t.Errorf("paused %d times, want 4", len(delays))
	}
	// No Retry-After: every pause falls back to the jittered exponential backoff,
	// which is bounded by the shared policy's ceiling.
	for i, d := range delays {
		if d <= 0 || d > 8*time.Second {
			t.Errorf("delay %d = %v, outside (0, 8s]", i, d)
		}
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
