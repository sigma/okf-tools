package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend/request"
)

// defaults for the shared Notion HTTP client. The base URL is overridable (an
// httptest server in tests); the Notion-Version pins the API surface this client
// speaks.
const (
	defaultBaseURL = "https://api.notion.com/v1"
	// The backend queries POST /data_sources/{id}/query and creates pages under
	// data_source_id parents — routes and payloads the 2025-09-03 data-source API
	// introduced. The pinned version must name that surface; an older version (e.g.
	// 2022-06-28) lacks those routes and fails scan with 400 invalid_request_url.
	defaultNotionVersion = "2025-09-03"
)

// The rate-limit defenses every Notion call inherits from the do chokepoint.
//
// DefaultInterval is the *global* sustained spacing between two Notion requests
// when no plan is stated — the Free/Plus budget, derived from its per-minute
// figure. Notion rate-limits a connection, not a page, so the gate is keyed on
// nothing: all traffic (scan, creates, appends, archives, write-back's property
// PATCHes) spends the one budget.
const (
	DefaultInterval = time.Minute / budgetStandard
	// defaultMaxAttempts bounds a single request's total tries — the first attempt
	// plus its retries. Exhausting it fails the run naming the status and the count.
	defaultMaxAttempts = 5
	// The retry delay policy — jittered exponential backoff and Retry-After
	// honouring — lives in the shared request package; these name it locally for
	// this client's tests.
	retryBaseBackoff = request.BaseBackoff
	maxRetryBackoff  = request.MaxBackoff
	// DefaultRequestTimeout bounds ONE attempt: how long the client waits for a
	// response before treating the request as stalled.
	//
	// Without it a publish has no deadline anywhere. HTTP/2 multiplexes every call
	// over one connection, so a half-dead connection parks the stream on a reply
	// that never arrives, and nothing at any layer gives up — a run observed in the
	// wild sat wedged for 34 minutes on 2.5 seconds of CPU (#184). The retry loop
	// cannot help on its own: it classifies STATUSES, and a request that never
	// returns produces none.
	//
	// Generous on purpose. The failure being fixed is unbounded, not slow: a
	// healthy Notion call answers in well under a second, and a bulk block append
	// on a bad day is still far inside this.
	DefaultRequestTimeout = request.DefaultTimeout
	// maxRetryAfter caps how long a server-sent Retry-After may park the run.
	// Bounding the attempts bounds nothing if one header can stall a publish for an
	// hour; retrying earlier than asked risks another 429, which is itself bounded
	// and reported.
	maxRetryAfter = request.MaxRetryAfter
)

// limiter is the Notion client's rate-limit policy: the global admission gate
// every request passes and the backoff schedule its retries follow. It is kept
// apart from the Backend's Notion domain state (block caps, schema, ids) because
// it models the transport's constraint, not Notion's content model.
//
// It admits traffic from ONE token bucket shaped like the limit Notion documents
// (sigma/okf-tools#210): a budget per minute, spendable at any pace within the
// window. The bucket holds a minute's worth of requests, starts full, and refills
// continuously at the sustained rate — so a burst on a fresh window waits for
// nothing, and a relentless caller settles at exactly the budget. Reads and writes
// spend the same tokens, because Notion meters the connection, not the route.
//
// Two policies preceded this one and are gone on purpose. #129 serialized every
// write at the interval, on the theory that a 429 mid-burst would cost a retry of
// something not replay-safe — but a 429 is by definition not applied, and the
// retry loop has always re-sent one on every route, so that margin bought only
// wall-clock. #134 then gave reads a small bucket of their own, which was the
// right shape applied to half the traffic. The remaining safety net is the one that
// was always doing the work: a 429 is bounded, retried, counted — and now also
// EMPTIES the bucket (drain), since the server has just said the window is spent.
//
// interval is the sustained spacing, one token per interval; non-positive disables
// pacing entirely. maxAttempts bounds one request's tries; now/sleep are the clock
// seam, real time by default and overridable so tests exercise pacing and backoff
// with no wall-clock delay.
//
// mu guards the bucket: tokens is its level and filled the instant it was last
// refilled. A waiter RESERVES its token before releasing mu, so concurrent callers
// queue rather than collide.
type limiter struct {
	interval    time.Duration
	maxAttempts int
	// timeout bounds one attempt's round trip. Non-positive disables the bound,
	// which is what the wedged run of #184 effectively ran with.
	timeout time.Duration
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
	mu      sync.Mutex
	tokens  float64
	filled  time.Time
}

// counters is the run's API traffic accounting: how many attempts the client made
// and how many of those it re-sent, split by why (#134). It lives beside the
// limiter because it measures the same thing the limiter governs — the request
// stream — and is read out through Backend.RequestStats at the end of a run.
//
// Every field counts ATTEMPTS, so a call retried twice contributes three requests
// and two retries. The mutex is what makes the counters safe to increment from the
// concurrent transport, which drives independent transactions in parallel.
type counters struct {
	mu        sync.Mutex
	requests  int
	throttled int
	transient int
}

// request records one HTTP attempt, whatever it returns.
func (c *counters) request() {
	c.mu.Lock()
	c.requests++
	c.mu.Unlock()
}

// retry records that an attempt which failed with status is being re-sent,
// attributing it to throttling (429) or to a transient server failure (5xx).
// stalledRetry books a re-send caused by THIS client's deadline firing rather
// than by a status. It lands in the transient bucket, which is what a stall is:
// the summary's transient count is every re-send that was not a 429 (#184).
func (c *counters) stalledRetry() {
	c.mu.Lock()
	c.transient++
	c.mu.Unlock()
}

func (c *counters) retry(status int) {
	c.mu.Lock()
	if status == http.StatusTooManyRequests {
		c.throttled++
	} else {
		c.transient++
	}
	c.mu.Unlock()
}

// snapshot reads the counters out as the neutral summary the pipeline reports.
func (c *counters) snapshot() publish.RequestStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return publish.RequestStats{Requests: c.requests, Throttled: c.throttled, Transient: c.transient}
}

// httpDoer is the one method the shared client needs from net/http. Narrowing to
// it lets a test inject an *http.Client whose Transport records requests, or any
// other doer, without the backend importing httptest.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// do performs one JSON request against the shared client and decodes the response
// body into out (nil to discard). It is the single choke point every Notion call
// trades through — Executor, Scanner, Provisioner and WriteBacker alike — so
// Notion's HTTP specifics never leak past this file, and so the rate-limit
// defenses live in exactly one place:
//
//   - pacing: every attempt passes the global admission gate first, so the whole
//     run (not just one page) spends Notion's per-minute budget;
//   - retry: a throttled (429) or transient (5xx) failure is retried with the
//     server's Retry-After or a jittered exponential backoff, bounded by
//     maxAttempts. Any other non-2xx is a bug in this client's request and fails
//     immediately with its status and body preserved.
func (b *Backend) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("notion: marshal %s %s: %w", method, path, err)
		}
		payload = buf
	}

	for attempt := 1; ; attempt++ {
		if err := b.limits.admit(ctx); err != nil {
			return fmt.Errorf("notion: %s %s: %w", method, path, err)
		}

		b.stats.request()
		status, header, data, err := b.attempt(ctx, method, path, payload)
		if err != nil {
			// A transport-level failure (connection reset, timeout) is NOT retried: the
			// request may have reached Notion and been applied, and unlike a 429 there is
			// no signal that it did not. Replaying a create on that guess mints a
			// duplicate page — see replaySafe.
			if !b.stalled(ctx, err) || !replaySafe(method, path) || attempt >= b.limits.maxAttempts {
				return err
			}
			// A stalled attempt on a replay-safe route is exactly what the retry loop
			// exists to absorb: replaying a *set* operation lands the same state whether
			// or not the first attempt reached Notion, so one bad connection costs a
			// backoff rather than the run (#184).
			b.stats.stalledRetry()
			delay := b.limits.retryDelay(nil, attempt)
			b.logf("notion: %s %s: no response within %v, retry %d/%d in %v",
				method, path, b.limits.timeout, attempt, b.limits.maxAttempts-1, delay)
			if err := b.limits.sleep(ctx, delay); err != nil {
				return fmt.Errorf("notion: %s %s: timeout, retry aborted: %w", method, path, err)
			}
			continue
		}

		switch {
		case status >= 200 && status < 300:
			if out != nil && len(data) > 0 {
				if err := json.Unmarshal(data, out); err != nil {
					return fmt.Errorf("notion: decode %s %s response: %w", method, path, err)
				}
			}
			return nil

		case !retryable(status, method, path):
			return fmt.Errorf("notion: %s %s: status %d: %s", method, path, status, string(data))

		case attempt >= b.limits.maxAttempts:
			return fmt.Errorf("notion: %s %s: status %d after %d attempts: %s",
				method, path, status, attempt, string(data))
		}

		b.stats.retry(status)
		if status == http.StatusTooManyRequests {
			b.limits.drain()
		}
		delay := b.limits.retryDelay(header, attempt)
		// A retry is a signal, not a silent recovery: a chronically throttled run must
		// be visible in its output rather than merely slow.
		b.logf("notion: %s %s: status %d, retry %d/%d in %v", method, path, status, attempt, b.limits.maxAttempts-1, delay)
		if err := b.limits.sleep(ctx, delay); err != nil {
			return fmt.Errorf("notion: %s %s: status %d, retry aborted: %w", method, path, status, err)
		}
	}
}

// attempt performs one HTTP round-trip and returns its status, headers and body.
// A returned error is a transport or request-construction failure, already
// wrapped; a non-2xx status is not an error here — classifying it is do's job.
func (b *Backend) attempt(ctx context.Context, method, path string, payload []byte) (int, http.Header, []byte, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}

	// The deadline is PER ATTEMPT, not per call: a retry gets its own full budget,
	// so a stall costs one backoff rather than eating the whole call's allowance
	// (#184). Cancelling covers the body read as well as the round trip — a
	// response whose body never finishes streaming hangs exactly as a missing
	// response does.
	// caller is kept UNDERIVED so a failure can be told apart from the caller
	// giving up: inside the derived context every error looks like a deadline.
	caller := ctx
	if b.limits.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, b.limits.timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, reader)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("notion: build request %s %s: %w", method, path, err)
	}
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	req.Header.Set("Notion-Version", b.notionVersion)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := b.http.Do(req)
	if err != nil {
		return 0, nil, nil, b.wrapAttemptErr(caller, method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, b.wrapAttemptErr(caller, "read "+method, path, err)
	}
	return resp.StatusCode, resp.Header, data, nil
}

// wrapAttemptErr names a failed round trip. A stall is reported as a TIMEOUT
// naming the budget it exceeded rather than as a bare "context deadline
// exceeded", because the two read very differently to whoever finds the failed
// run: one says the destination stopped answering, the other looks like a bug in
// this program.
func (b *Backend) wrapAttemptErr(ctx context.Context, method, path string, err error) error {
	return request.WrapErr(ctx, "notion", method, path, b.limits.timeout, err)
}

// stalled reports whether err is THIS client's per-attempt deadline firing rather
// than the caller giving up. The distinction decides whether a retry is even
// considered: a cancelled run stops, it does not back off and try again.
func (b *Backend) stalled(ctx context.Context, err error) bool {
	return request.Stalled(ctx, err)
}

// retryable reports whether a failed request should be retried, given its status
// and which route it targeted.
//
// Only 429 and 5xx qualify: any other 4xx is a bug in the request this client
// built (a malformed body, a property the page does not have — #128), and
// retrying it only turns a fast, diagnosable failure into a slow one.
func retryable(status int, method, path string) bool {
	switch {
	case status == http.StatusTooManyRequests:
		// A 429 is refused *at the rate limiter*, before Notion looks at the payload,
		// so the write provably did not happen — even a page create, which is not
		// idempotent, cannot have minted a page. That is what makes replaying it safe
		// here while a 5xx replay is not. Do not weaken this reasoning to "creates are
		// retryable"; it holds only because the failure precedes the write. (It rests
		// on 429 meaning "refused, not processed" for whatever answered — Notion's own
		// limiter does; an intervening gateway is assumed to, since a 429 that had
		// applied the write would be indistinguishable from one that had not.)
		return true
	case status >= 500 && status < 600:
		// A 5xx carries no such guarantee: Notion may have applied the write and failed
		// afterwards. Only requests that can be replayed without duplicating an effect
		// qualify.
		return replaySafe(method, path)
	default:
		return false
	}
}

// replaySafe reports whether re-sending this request can never duplicate an
// effect — the condition for retrying it after a failure that may have landed.
//
// It is an explicit allow-list of routes, and it fails CLOSED: a route it does
// not recognize is unsafe. A new call added to this client is therefore never
// silently replayed — whoever adds it must come here and state why replaying it
// cannot duplicate anything.
//
// Reads are trivially safe whatever they target. Among the writes, only the *set*
// operations are: PATCH /pages/{id} overwrites properties (or archives), PATCH
// /blocks/{id} overwrites a block's content, PATCH /data_sources/{id} declares
// columns — replaying any of them lands the same state. The two *append*
// operations are deliberately absent: POST /pages mints a new page and PATCH
// /blocks/{id}/children appends new blocks, so a replay whose original attempt
// actually succeeded duplicates a page or a block.
func replaySafe(method, path string) bool {
	if method == http.MethodGet {
		return true
	}
	seg := routeSegments(path)
	switch method {
	case http.MethodPost:
		// The data-source query is a read in POST's clothing (its body carries the
		// cursor). POST /pages — the create — is absent by design.
		return isQueryRoute(seg)
	case http.MethodPatch:
		// The single-object set writes. PATCH /blocks/{id}/children — the append — has
		// three segments and so never matches.
		return len(seg) == 2 && (seg[0] == "pages" || seg[0] == "blocks" || seg[0] == "data_sources")
	case http.MethodDelete:
		// DELETE /blocks/{id} archives one block — a set, like the PATCHes above:
		// replaying it lands the same state (the block stays archived) and can
		// duplicate nothing. It is the per-child call a content replacement issues
		// before appending the new body (#130).
		return len(seg) == 2 && seg[0] == "blocks"
	default:
		return false
	}
}

// isQueryRoute reports whether these segments name the data-source query, the one
// POST that only reads — and so the one POST replaySafe may re-send, since
// replaying a query duplicates nothing.
func isQueryRoute(seg []string) bool {
	return len(seg) == 3 && seg[0] == "data_sources" && seg[2] == "query"
}

// routeSegments splits a request path into its non-empty segments, dropping any
// query string, so a route is classified by its shape rather than by matching
// text that a cursor or page-size parameter could shift.
func routeSegments(path string) []string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	return strings.FieldsFunc(path, func(r rune) bool { return r == '/' })
}

// retryDelay picks how long to wait before the next attempt: the server's
// Retry-After when it sent one (Notion sends it on a 429), otherwise an
// exponential backoff doubling from retryBaseBackoff up to maxRetryBackoff, with
// full jitter so a fleet of retries does not re-collide in lockstep.
func (l *limiter) retryDelay(header http.Header, attempt int) time.Duration {
	return request.Delay(header, attempt, l.now)
}

// retryAfter reads a Retry-After header in either of its two forms — a delay in
// seconds, or an HTTP-date — and reports the delay it asks for, clamped to
// maxRetryAfter. A missing, unparseable, or already-elapsed value reports false so
// the caller backs off instead.
func (l *limiter) retryAfter(header http.Header) (time.Duration, bool) {
	return request.RetryAfter(header, l.now)
}

// capacity is the bucket's size: one minute's budget at the sustained rate, and
// never less than one token, so a very long interval still admits a request.
func (l *limiter) capacity() float64 {
	return max(1, float64(time.Minute/l.interval))
}

// admit takes one token from the bucket, waiting only when it is empty. The
// bucket starts full, refills continuously at the sustained rate the interval
// names (one token per interval), and is capped at capacity — so a fresh window
// admits a whole budget's worth back to back, and a run that has spent it settles
// at exactly the sustained rate. A non-positive interval disables pacing entirely.
//
// A waiter reserves its token by advancing the refill clock past the wait before
// releasing mu, so two callers that both find the bucket empty wait different
// amounts and are admitted in turn instead of together.
func (l *limiter) admit(ctx context.Context) error {
	if l.interval <= 0 {
		return nil
	}
	l.mu.Lock()
	now := l.now()
	if l.filled.IsZero() {
		l.filled, l.tokens = now, l.capacity()
	}
	l.tokens = min(l.capacity(), l.tokens+float64(now.Sub(l.filled))/float64(l.interval))
	l.filled = now

	var wait time.Duration
	if l.tokens < 1 {
		// tokens goes negative as waiters queue (each parks `filled` past its own
		// wait), so this product grows with queue depth; clamp it so an absurd
		// interval cannot overflow the float→Duration conversion into a negative wait.
		// Rounded UP to the nanosecond: a float wait truncated short by a hair would
		// admit a request a hair before its token exists.
		if w := (1 - l.tokens) * float64(l.interval); w >= float64(math.MaxInt64) {
			wait = time.Duration(math.MaxInt64)
		} else {
			wait = time.Duration(math.Ceil(w))
		}
		l.tokens, l.filled = 1, now.Add(wait)
	}
	l.tokens--
	l.mu.Unlock()

	if wait <= 0 {
		return ctx.Err()
	}
	return l.sleep(ctx, wait)
}

// drain empties the bucket: the response to a 429. Whatever the client believed
// the window still held, the server has said otherwise, and the honest level is
// zero — refilling from now at the sustained rate. The throttled request itself
// then waits the server's Retry-After (see do), which refills a little headroom
// for it; the requests behind it pay refill rather than resuming a burst the
// server has already refused.
func (l *limiter) drain() {
	if l.interval <= 0 {
		return
	}
	l.mu.Lock()
	// A bucket already below zero is one with waiters parked on it, each holding a
	// reservation against a refill clock set in the future. Those stand: raising
	// the level to zero would let new arrivals draw alongside them, admitting MORE
	// after a 429 than before it.
	if l.tokens > 0 {
		l.tokens, l.filled = 0, l.now()
	}
	l.mu.Unlock()
}

// --- wire types shared by the Executor and Scanner --------------------------

// object is any Notion object carrying at least an id — a created page, an
// appended block, a listed child. The Executor reads the id back to seed the
// resolution table. Type is the block type, present when the object came from a
// children listing (empty on a create/append echo, which needs only the id): a
// content assertion reads it to leave a child_page — a node of its own — alone.
type object struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// pageParent is the parent a page-create attaches to: a data-source row for a
// top-level node, or a page for a cluster subpage.
type pageParent struct {
	Type         string `json:"type"`
	DataSourceID string `json:"data_source_id,omitempty"`
	PageID       string `json:"page_id,omitempty"`
}

// createPageReq is the POST /pages body: the fused create + properties + first
// content chunk.
type createPageReq struct {
	Parent     pageParent       `json:"parent"`
	Properties map[string]any   `json:"properties"`
	Children   []map[string]any `json:"children,omitempty"`
}

// appendChildrenReq is the PATCH /blocks/{id}/children body: an overflow content
// batch appended to an existing page.
type appendChildrenReq struct {
	Children []map[string]any `json:"children"`
}

// updatePageReq is the PATCH /pages/{id} body: a standalone property update, or an
// archive (Archived = true) for a DeleteNode.
type updatePageReq struct {
	Properties map[string]any `json:"properties,omitempty"`
	Archived   *bool          `json:"archived,omitempty"`
}

// childrenList is the paginated GET /blocks/{id}/children response: the ordered
// child block objects, matched positionally to the children the create sent so the
// anchor map can be built.
type childrenList struct {
	Results    []object `json:"results"`
	NextCursor string   `json:"next_cursor"`
	HasMore    bool     `json:"has_more"`
}

// appendResult is the PATCH /blocks/{id}/children response: the appended blocks in
// order, their ids used to map any hosted anchors.
type appendResult struct {
	Results []object `json:"results"`
}

// --- the scan's data-source query wire types --------------------------------

// queryReq is the paginated POST /data_sources/{id}/query body.
type queryReq struct {
	StartCursor string `json:"start_cursor,omitempty"`
	PageSize    int    `json:"page_size,omitempty"`
}

// queryResp is one page of a data-source query: the top-level rows plus the cursor
// to the next page.
type queryResp struct {
	Results    []queryRow `json:"results"`
	NextCursor string     `json:"next_cursor"`
	HasMore    bool       `json:"has_more"`
}

// queryRow is one top-level data-source row: its Notion page id and the
// self-describing derived-column properties the ScanStored path reads.
type queryRow struct {
	ID         string              `json:"id"`
	Properties map[string]property `json:"properties"`
}

// property is the sliver of a Notion property object ScanStored needs: a typed
// value whose plain text carries a derived column (path, hash, or a JSON-encoded
// subtree / anchor map). Only title and rich_text are read — the shapes the
// derived columns use.
type property struct {
	Type     string     `json:"type"`
	Title    []richText `json:"title"`
	RichText []richText `json:"rich_text"`
}

// richText is one span of a Notion rich-text / title value; plainText concatenates
// the spans' content.
type richText struct {
	PlainText string `json:"plain_text"`
	Text      struct {
		Content string `json:"content"`
	} `json:"text"`
}

// plainText flattens a property to its plain string: the concatenated content of
// its title or rich_text spans, preferring the server-provided plain_text and
// falling back to the authored text content. An empty or absent property yields "".
func plainText(p property) string {
	spans := p.RichText
	if len(spans) == 0 {
		spans = p.Title
	}
	var out string
	for _, s := range spans {
		if s.PlainText != "" {
			out += s.PlainText
		} else {
			out += s.Text.Content
		}
	}
	return out
}

// RequestStats reports what this backend's traffic has cost so far: every HTTP
// attempt it made, and how many of those it re-sent after a 429 or a 5xx. The
// pipeline reads it once at the end of a run so the summary can distinguish a run
// that was slow because it was throttled from one that was slow because it issued
// thousands of requests (#134). It is safe to call while requests are in flight.
func (b *Backend) RequestStats() publish.RequestStats { return b.stats.snapshot() }
