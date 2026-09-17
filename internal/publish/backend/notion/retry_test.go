package notion

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/optimize"
	"github.com/sigma/okf-tools/internal/publish/transport"
)

// --- harness ----------------------------------------------------------------

// stubResp is one canned HTTP reply the stub server hands out.
type stubResp struct {
	status int
	body   string
	header map[string]string
}

// stubServer serves a canned sequence of replies (the last one repeats once the
// sequence is exhausted) and records every request, so a test can assert exactly
// how many attempts the client made and against which route.
type stubServer struct {
	mu    sync.Mutex
	resps []stubResp
	calls []string
	next  int
}

func (s *stubServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	resp := s.resps[min(s.next, len(s.resps)-1)]
	s.next++
	s.mu.Unlock()

	for k, v := range resp.header {
		w.Header().Set(k, v)
	}
	w.WriteHeader(resp.status)
	_, _ = io.WriteString(w, resp.body)
}

func (s *stubServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// fakeClock is the virtual clock the retry/pacing tests drive: a sleep is
// recorded rather than served, and advances virtual time by its duration exactly
// as a real one would.
type fakeClock struct {
	t     time.Time
	slept []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) doSleep(_ context.Context, d time.Duration) error {
	c.slept = append(c.slept, d)
	c.t = c.t.Add(d)
	return nil
}

// newStub wires a backend to a stub server serving resps, on a virtual clock and
// with pacing disabled unless a test option re-enables it.
func newStub(t *testing.T, opts []Option, resps ...stubResp) (*Backend, *stubServer, *fakeClock) {
	t.Helper()
	s := &stubServer{resps: resps}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	clock := newFakeClock()
	base := []Option{
		WithBaseURL(srv.URL), WithToken("tok"), WithDataSourceID("ds1"),
		WithInterval(0), withClock(clock.now, clock.doSleep),
		WithLogger(func(string, ...any) {}),
	}
	return New(append(base, opts...)...), s, clock
}

// --- retry ------------------------------------------------------------------

// A 429 is retried and the call ultimately succeeds, offline against the stub.
func TestRetriesAfter429(t *testing.T) {
	be, s, _ := newStub(t, nil,
		stubResp{status: http.StatusTooManyRequests, body: `{"message":"rate limited"}`},
		stubResp{status: http.StatusOK, body: `{"id":"page-1"}`},
	)

	var out object
	if err := be.do(context.Background(), http.MethodGet, "/pages/x", nil, &out); err != nil {
		t.Fatalf("do: %v", err)
	}
	if out.ID != "page-1" {
		t.Errorf("id = %q, want page-1", out.ID)
	}
	if got := s.count(); got != 2 {
		t.Errorf("requests = %d, want 2 (one 429 + one retry)", got)
	}
}

// A transient 5xx on a replay-safe request is retried too.
func TestRetriesAfter5xx(t *testing.T) {
	be, s, _ := newStub(t, nil,
		stubResp{status: http.StatusBadGateway, body: `upstream down`},
		stubResp{status: http.StatusOK, body: `{"id":"page-1"}`},
	)

	if err := be.do(context.Background(), http.MethodGet, "/pages/x", nil, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	if got := s.count(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}

// A Retry-After header in seconds is honored verbatim rather than backed off.
func TestHonorsRetryAfterSeconds(t *testing.T) {
	be, _, clock := newStub(t, nil,
		stubResp{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "2"}},
		stubResp{status: http.StatusOK, body: `{}`},
	)

	if err := be.do(context.Background(), http.MethodGet, "/pages/x", nil, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	if len(clock.slept) != 1 || clock.slept[0] != 2*time.Second {
		t.Errorf("slept = %v, want [2s]", clock.slept)
	}
}

// A Retry-After given as an HTTP-date is honored as the delay to that instant.
func TestHonorsRetryAfterDate(t *testing.T) {
	clockBase := newFakeClock().now()
	be, _, clock := newStub(t, nil,
		stubResp{status: http.StatusServiceUnavailable, header: map[string]string{
			"Retry-After": clockBase.Add(3 * time.Second).Format(http.TimeFormat),
		}},
		stubResp{status: http.StatusOK, body: `{}`},
	)

	if err := be.do(context.Background(), http.MethodGet, "/pages/x", nil, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	if len(clock.slept) != 1 || clock.slept[0] != 3*time.Second {
		t.Errorf("slept = %v, want [3s]", clock.slept)
	}
}

// Without a Retry-After the delay backs off exponentially, jittered but bounded.
func TestBackoffGrowsAndIsBounded(t *testing.T) {
	be, _, clock := newStub(t, nil,
		stubResp{status: http.StatusServiceUnavailable, body: `down`},
	)

	err := be.do(context.Background(), http.MethodGet, "/pages/x", nil, nil)
	if err == nil {
		t.Fatal("want error after exhausting attempts")
	}
	if len(clock.slept) != defaultMaxAttempts-1 {
		t.Fatalf("slept %d times, want %d", len(clock.slept), defaultMaxAttempts-1)
	}
	for i, d := range clock.slept {
		base := retryBaseBackoff << i
		if base > maxRetryBackoff {
			base = maxRetryBackoff
		}
		if d <= 0 || d > base {
			t.Errorf("backoff %d = %v, want within (0, %v]", i, d, base)
		}
	}
}

// A 400 is a bug in okfpub's request, not a transient failure: it fails on the
// first attempt with the status and the response body preserved.
func TestDoesNotRetry400(t *testing.T) {
	be, s, _ := newStub(t, nil,
		stubResp{status: http.StatusBadRequest, body: `{"message":"path is not a property that exists"}`},
	)

	err := be.do(context.Background(), http.MethodGet, "/pages/x", nil, nil)
	if err == nil {
		t.Fatal("want error")
	}
	if got := s.count(); got != 1 {
		t.Errorf("requests = %d, want 1 (a 400 must not be retried)", got)
	}
	if !strings.Contains(err.Error(), "status 400") || !strings.Contains(err.Error(), "is not a property that exists") {
		t.Errorf("error = %q, want the status and the response body", err)
	}
}

// Retries are bounded, and exhausting them names the status and the attempt count.
func TestRetriesAreBounded(t *testing.T) {
	be, s, _ := newStub(t, nil,
		stubResp{status: http.StatusServiceUnavailable, body: `overloaded`},
	)

	err := be.do(context.Background(), http.MethodGet, "/pages/x", nil, nil)
	if err == nil {
		t.Fatal("want error")
	}
	if got := s.count(); got != defaultMaxAttempts {
		t.Errorf("requests = %d, want %d", got, defaultMaxAttempts)
	}
	for _, want := range []string{"status 503", "5 attempts", "overloaded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// A retry is a signal, not a silent recovery: each one is reported to the run's
// output naming the status and the route.
func TestRetryIsVisibleInOutput(t *testing.T) {
	var lines []string
	be, _, _ := newStub(t, []Option{WithLogger(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})},
		stubResp{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "1"}},
		stubResp{status: http.StatusOK, body: `{}`},
	)

	if err := be.do(context.Background(), http.MethodGet, "/pages/x", nil, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("reported %d line(s), want 1: %v", len(lines), lines)
	}
	for _, want := range []string{"429", "/pages/x", "retry"} {
		if !strings.Contains(strings.ToLower(lines[0]), strings.ToLower(want)) {
			t.Errorf("report = %q, want it to mention %q", lines[0], want)
		}
	}
}

// --- retry safety of non-idempotent writes ----------------------------------

// A page create is not idempotent: a 5xx may have been raised after the page was
// minted, so replaying it would duplicate the page. It fails fast instead.
func TestCreateNotRetriedOn5xx(t *testing.T) {
	be, s, _ := newStub(t, nil, stubResp{status: http.StatusInternalServerError, body: `boom`})

	err := be.do(context.Background(), http.MethodPost, "/pages", createPageReq{}, nil)
	if err == nil {
		t.Fatal("want error")
	}
	if got := s.count(); got != 1 {
		t.Errorf("requests = %d, want 1 (a create must not be replayed after a 5xx)", got)
	}
}

// A block append is not idempotent either — replaying it duplicates the blocks.
func TestAppendNotRetriedOn5xx(t *testing.T) {
	be, s, _ := newStub(t, nil, stubResp{status: http.StatusBadGateway, body: `boom`})

	err := be.do(context.Background(), http.MethodPatch, "/blocks/b1/children", appendChildrenReq{}, nil)
	if err == nil {
		t.Fatal("want error")
	}
	if got := s.count(); got != 1 {
		t.Errorf("requests = %d, want 1 (an append must not be replayed after a 5xx)", got)
	}
}

// A 429 IS retried even for a create: Notion rejected the request at the rate
// limiter before applying it, so no page can have been minted.
func TestCreateRetriedOn429(t *testing.T) {
	be, s, _ := newStub(t, nil,
		stubResp{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "1"}},
		stubResp{status: http.StatusOK, body: `{"id":"page-1"}`},
	)

	var out object
	if err := be.do(context.Background(), http.MethodPost, "/pages", createPageReq{}, &out); err != nil {
		t.Fatalf("do: %v", err)
	}
	if out.ID != "page-1" {
		t.Errorf("id = %q, want page-1", out.ID)
	}
	if got := s.count(); got != 2 {
		t.Errorf("requests = %d, want 2 (a 429 precedes the write, so a create is replay-safe)", got)
	}
}

// An over-long Retry-After is clamped: bounded attempts bound nothing if one
// header can park the publish for an hour.
func TestRetryAfterIsClamped(t *testing.T) {
	be, _, clock := newStub(t, nil,
		stubResp{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "3600"}},
		stubResp{status: http.StatusOK, body: `{}`},
	)

	if err := be.do(context.Background(), http.MethodGet, "/pages/x", nil, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	if len(clock.slept) != 1 || clock.slept[0] != maxRetryAfter {
		t.Errorf("slept = %v, want [%v]", clock.slept, maxRetryAfter)
	}
}

// Route classification fails closed: a route the allow-list does not name is
// treated as unsafe to replay, so a future call is never silently retried.
func TestUnknownRouteNotRetriedOn5xx(t *testing.T) {
	be, s, _ := newStub(t, nil, stubResp{status: http.StatusServiceUnavailable, body: `down`})

	if err := be.do(context.Background(), http.MethodPatch, "/comments/c1", nil, nil); err == nil {
		t.Fatal("want error")
	}
	if got := s.count(); got != 1 {
		t.Errorf("requests = %d, want 1 (an unrecognized route must not be replayed)", got)
	}
}

// Classification reads the route's shape, not its text: a query string does not
// change what a request is.
func TestQueryStringDoesNotChangeClassification(t *testing.T) {
	be, s, _ := newStub(t, nil,
		stubResp{status: http.StatusServiceUnavailable, body: `down`},
		stubResp{status: http.StatusOK, body: `{}`},
	)

	if err := be.do(context.Background(), http.MethodPatch, "/pages/page-1?foo=1", updatePageReq{}, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	if got := s.count(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}

// A property PATCH is a last-write-wins set, so it is replay-safe on a 5xx.
func TestPropertyPatchRetriedOn5xx(t *testing.T) {
	be, s, _ := newStub(t, nil,
		stubResp{status: http.StatusServiceUnavailable, body: `down`},
		stubResp{status: http.StatusOK, body: `{"id":"page-1"}`},
	)

	if err := be.do(context.Background(), http.MethodPatch, "/pages/page-1", updatePageReq{}, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	if got := s.count(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}

// --- pacing (#210) ----------------------------------------------------------

// The bucket is sized to the plan's per-minute budget: a burst on a fresh window
// waits for nothing, and only the request past the budget pays — one interval,
// the sustained rate the budget names.
func TestBurstWithinTheBudgetIsFree(t *testing.T) {
	be, _, clock := newStub(t, []Option{WithPlan(PlanFree)}, stubResp{status: http.StatusOK, body: `{}`})
	budget := PlanFree.Budget()

	ctx := context.Background()
	for i := 0; i < budget; i++ {
		if err := be.do(ctx, http.MethodPatch, "/pages/x", nil, nil); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if len(clock.slept) != 0 {
		t.Fatalf("%d requests within a %d/min budget slept %v, want no wait at all", budget, budget, clock.slept)
	}
	if err := be.do(ctx, http.MethodPatch, "/pages/x", nil, nil); err != nil {
		t.Fatalf("write past the budget: %v", err)
	}
	if want := PlanFree.Interval(); len(clock.slept) != 1 || clock.slept[0] != want {
		t.Errorf("request %d slept %v, want one wait of %v (the sustained rate)", budget+1, clock.slept, want)
	}
}

// Reads and writes spend the same budget: Notion meters the connection, not the
// route, so a read after a write-exhausted window waits exactly as a write would.
func TestReadsAndWritesShareTheBucket(t *testing.T) {
	// One token per minute: the bucket holds exactly one request.
	interval := time.Minute
	be, _, clock := newStub(t, []Option{WithInterval(interval)}, stubResp{status: http.StatusOK, body: `{}`})

	ctx := context.Background()
	if err := be.do(ctx, http.MethodPatch, "/pages/x", nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := be.do(ctx, http.MethodGet, "/pages/x", nil, nil); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(clock.slept) != 1 || clock.slept[0] != interval {
		t.Errorf("the read after a spent bucket slept %v, want [%v]", clock.slept, interval)
	}
}

// A 429 empties the bucket: the server has said the window is spent, so what the
// client believed it still had is gone. The throttled request waits the server's
// Retry-After as before; the ones after it pay refill instead of bursting.
func TestThrottleEmptiesTheBucket(t *testing.T) {
	be, _, clock := newStub(t, []Option{WithPlan(PlanFree)},
		stubResp{status: http.StatusTooManyRequests, body: `{}`, header: map[string]string{"Retry-After": "1"}},
		stubResp{status: http.StatusOK, body: `{}`},
		stubResp{status: http.StatusOK, body: `{}`},
	)
	ctx := context.Background()
	if err := be.do(ctx, http.MethodPatch, "/pages/x", nil, nil); err != nil {
		t.Fatalf("throttled write: %v", err)
	}
	if len(clock.slept) != 1 || clock.slept[0] != time.Second {
		t.Fatalf("slept %v, want the 1s Retry-After alone", clock.slept)
	}
	// The retry-after refilled 3 tokens (1s at 3/s); the retry spent one. Two more
	// requests spend the rest, and the next waits for refill — not the 178 the fresh
	// bucket would still have held.
	for i := 0; i < 2; i++ {
		if err := be.do(ctx, http.MethodGet, "/pages/x", nil, nil); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if len(clock.slept) != 1 {
		t.Fatalf("reads within the refilled headroom slept %v, want nothing new", clock.slept[1:])
	}
	if err := be.do(ctx, http.MethodGet, "/pages/x", nil, nil); err != nil {
		t.Fatalf("read past the refill: %v", err)
	}
	if want := PlanFree.Interval(); len(clock.slept) != 2 || clock.slept[1] != want {
		t.Errorf("post-throttle request slept %v, want a refill wait of %v", clock.slept[1:], want)
	}
}

// Interval 0 disables pacing outright — the setting the offline tests run under.
func TestZeroIntervalNeverPaces(t *testing.T) {
	be, _, clock := newStub(t, nil, stubResp{status: http.StatusOK, body: `{}`})

	for i := 0; i < 3; i++ {
		if err := be.do(context.Background(), http.MethodGet, "/pages/x", nil, nil); err != nil {
			t.Fatalf("do: %v", err)
		}
	}
	if len(clock.slept) != 0 {
		t.Errorf("interval 0 should never sleep, slept %v", clock.slept)
	}
}

// New paces at the Free/Plus budget unless told the plan — the rate every
// workspace is allowed, derived from the documented per-minute figure rather
// than a second table of durations.
func TestDefaultPacingIsTheFreeTierBudget(t *testing.T) {
	if got := New().limits.interval; got != DefaultInterval {
		t.Errorf("default interval = %v, want %v", got, DefaultInterval)
	}
	if DefaultInterval != PlanFree.Interval() {
		t.Errorf("DefaultInterval = %v, want the free tier's %v", DefaultInterval, PlanFree.Interval())
	}
	if PlanFree.Budget() != 180 || PlanPlus.Budget() != 180 || PlanBusiness.Budget() != 600 || PlanEnterprise.Budget() != 600 {
		t.Errorf("budgets = %d/%d/%d/%d, want Notion's documented 180/180/600/600 per minute",
			PlanFree.Budget(), PlanPlus.Budget(), PlanBusiness.Budget(), PlanEnterprise.Budget())
	}
}

// WithPlan sets the sustained rate from the plan's budget, and a WithInterval
// applied after it overrides it — options apply in order, which is how the
// pipeline expresses "explicit interval beats plan".
func TestPlanSetsTheIntervalAndAnExplicitIntervalOverridesIt(t *testing.T) {
	if got := New(WithPlan(PlanBusiness)).limits.interval; got != PlanBusiness.Interval() {
		t.Errorf("business interval = %v, want %v", got, PlanBusiness.Interval())
	}
	if got := New(WithPlan(PlanBusiness), WithInterval(time.Second)).limits.interval; got != time.Second {
		t.Errorf("interval after plan = %v, want the explicit 1s", got)
	}
}

// ParsePlan accepts the four documented plans, case-insensitively, and names them
// all on anything else — the message a usage error shows.
func TestParsePlan(t *testing.T) {
	for in, want := range map[string]Plan{"free": PlanFree, "Plus": PlanPlus, "BUSINESS": PlanBusiness, "enterprise": PlanEnterprise} {
		if got, err := ParsePlan(in); err != nil || got != want {
			t.Errorf("ParsePlan(%q) = (%q, %v), want %q", in, got, err, want)
		}
	}
	_, err := ParsePlan("team")
	if err == nil {
		t.Fatal("ParsePlan(team) should fail")
	}
	for _, name := range []string{"free", "plus", "business", "enterprise"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name the accepted plan %q", err, name)
		}
	}
}

// --- end to end -------------------------------------------------------------

// A throttled workspace no longer fails the run: the faked Notion surface
// rate-limits the first requests of a publish, the client waits out each
// Retry-After, and the publish completes — minting exactly one page, with every
// retry reported.
func TestThrottledPublishSucceeds(t *testing.T) {
	f := newFakeNotion()
	f.throttle = 3 // the run's first three requests come back 429
	clock := newFakeClock()
	var reports []string
	be := newServer(t, f,
		withClock(clock.now, clock.doSleep),
		WithLogger(func(format string, args ...any) {
			reports = append(reports, fmt.Sprintf(format, args...))
		}))

	g := &graph.Graph{Ops: []*graph.Op{
		createOp("node:a.md"), propsOp("node:a.md"),
		contentOpBlocks("node:a.md", publish.Block{Content: para(txt("Hello"))}),
	}}
	dag := optimize.Optimize(g, be, be)

	res, err := transport.New(be).Run(context.Background(), dag, publish.NewCurrentState(nil, nil, nil))
	if err != nil {
		t.Fatalf("throttled publish should have recovered, got: %v", err)
	}
	if _, ok := res.Nodes["node:a.md"]; !ok {
		t.Error("node:a.md never resolved")
	}
	// The retries must not have duplicated the page the create minted.
	if got := f.countPath(http.MethodPost, "/pages"); got != 1 {
		t.Errorf("recorded %d page creates, want exactly 1", got)
	}
	if len(reports) != 3 {
		t.Errorf("reported %d retries, want 3: %v", len(reports), reports)
	}
	for i, d := range clock.slept {
		if d != time.Second {
			t.Errorf("sleep %d = %v, want the 1s Retry-After the fake sent", i, d)
		}
	}
}

// Write-back's property PATCHes pass through the same gate: they spend the
// budget too.
func TestWriteBackIsPaced(t *testing.T) {
	f := newFakeNotion()
	clock := newFakeClock()
	interval := time.Minute // a one-token bucket
	be := newServer(t, f, WithInterval(interval), withClock(clock.now, clock.doSleep))

	prov := publish.Provenance{Nodes: map[publish.SymbolicID]publish.NodeProvenance{
		"node:a.md": {ID: "page-a", NodeStamp: publish.NodeStamp{Hash: "h1"}},
		"node:b.md": {ID: "page-b", NodeStamp: publish.NodeStamp{Hash: "h2"}},
	}}
	if err := be.WriteBack(context.Background(), prov); err != nil {
		t.Fatalf("write-back: %v", err)
	}

	// Two PATCHes: the second one waits for the bucket to refill behind the first.
	if len(clock.slept) != 1 || clock.slept[0] != interval {
		t.Errorf("slept = %v, want [%v] — write-back must be paced too", clock.slept, interval)
	}
}

// --- request accounting (#134) ----------------------------------------------

// The run's request count includes every attempt, and a retried 429 is counted as
// throttling — the two numbers that tell "slow because throttled" from "slow
// because it issued thousands of requests".
func TestRequestStatsCountAttemptsAndRetries(t *testing.T) {
	be, _, _ := newStub(t, nil,
		stubResp{status: http.StatusTooManyRequests, body: `{}`},
		stubResp{status: http.StatusServiceUnavailable, body: `{}`},
		stubResp{status: http.StatusOK, body: `{}`},
	)

	if err := be.do(context.Background(), http.MethodGet, "/pages/x", nil, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	got := be.RequestStats()
	want := publish.RequestStats{Requests: 3, Throttled: 1, Transient: 1}
	if got != want {
		t.Errorf("stats = %+v, want %+v", got, want)
	}
}

// A backend that issued nothing reports zeros rather than nothing.
func TestRequestStatsStartAtZero(t *testing.T) {
	be, _, _ := newStub(t, nil, stubResp{status: http.StatusOK, body: `{}`})
	if got := (be.RequestStats()); got != (publish.RequestStats{}) {
		t.Errorf("stats = %+v, want zeros", got)
	}
}

// The reported request count is the truth, not an estimate: it equals what the
// server actually received across a whole publish — retried attempts included.
func TestRequestStatsMatchWhatTheServerReceived(t *testing.T) {
	f := newFakeNotion()
	f.throttle = 3 // the run's first three requests come back 429
	clock := newFakeClock()
	be := newServer(t, f, withClock(clock.now, clock.doSleep), WithLogger(func(string, ...any) {}))

	g := &graph.Graph{Ops: []*graph.Op{
		createOp("node:a.md"), propsOp("node:a.md"),
		contentOpBlocks("node:a.md", publish.Block{Content: para(txt("Hello"))}),
	}}
	dag := optimize.Optimize(g, be, be)
	if _, err := transport.New(be).Run(context.Background(), dag, publish.NewCurrentState(nil, nil, nil)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got := be.RequestStats()
	f.mu.Lock()
	received := f.received
	f.mu.Unlock()

	if got.Requests != received {
		t.Errorf("reported %d request(s), server received %d", got.Requests, received)
	}
	if got.Requests == 0 {
		t.Fatal("a publish reported zero requests")
	}
	if got.Throttled != 3 {
		t.Errorf("throttled = %d, want the 3 injected 429s", got.Throttled)
	}
	if got.Transient != 0 {
		t.Errorf("transient = %d, want 0 — the fake served no 5xx", got.Transient)
	}
}
