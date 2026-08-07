package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
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
// DefaultInterval is the *global* minimum spacing between two Notion requests —
// Notion rate-limits an integration by requests per second, not per page, so this
// is keyed on nothing: all traffic (scan, creates, appends, archives, write-back's
// property PATCHes) queues behind the one gate.
const (
	DefaultInterval = 350 * time.Millisecond
	// defaultMaxAttempts bounds a single request's total tries — the first attempt
	// plus its retries. Exhausting it fails the run naming the status and the count.
	defaultMaxAttempts = 5
	// retryBaseBackoff is the first retry's delay ceiling, doubling per attempt up
	// to maxRetryBackoff. Used only when the response carries no Retry-After.
	retryBaseBackoff = 500 * time.Millisecond
	maxRetryBackoff  = 8 * time.Second
	// maxRetryAfter caps how long a server-sent Retry-After may park the run.
	// Bounding the attempts bounds nothing if one header can stall a publish for an
	// hour; retrying earlier than asked risks another 429, which is itself bounded
	// and reported.
	maxRetryAfter = 60 * time.Second
)

// limiter is the Notion client's rate-limit policy: the global admission gate
// every request passes and the backoff schedule its retries follow. It is kept
// apart from the Backend's Notion domain state (block caps, schema, ids) because
// it models the transport's constraint, not Notion's content model.
//
// interval is the minimum spacing between two admitted requests (non-positive
// disables pacing); maxAttempts bounds one request's tries; now/sleep are the
// clock seam, real time by default and overridable so tests exercise pacing and
// backoff with no wall-clock delay. mu/lastReq are the gate's state: lastReq is
// the instant the most recently admitted request was (or will be) issued.
type limiter struct {
	interval    time.Duration
	maxAttempts int
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
	mu          sync.Mutex
	lastReq     time.Time
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
//     run (not just one page) respects Notion's requests-per-second limit;
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
		if err := b.limits.pace(ctx); err != nil {
			return fmt.Errorf("notion: %s %s: %w", method, path, err)
		}

		status, header, data, err := b.attempt(ctx, method, path, payload)
		if err != nil {
			// A transport-level failure (connection reset, timeout) is NOT retried: the
			// request may have reached Notion and been applied, and unlike a 429 there is
			// no signal that it did not. Replaying a create on that guess mints a
			// duplicate page — see replaySafe.
			return err
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
		return 0, nil, nil, fmt.Errorf("notion: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("notion: read %s %s response: %w", method, path, err)
	}
	return resp.StatusCode, resp.Header, data, nil
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
		return len(seg) == 3 && seg[0] == "data_sources" && seg[2] == "query"
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
	if d, ok := l.retryAfter(header); ok {
		return d
	}
	backoff := retryBaseBackoff << (attempt - 1)
	if backoff > maxRetryBackoff || backoff <= 0 {
		backoff = maxRetryBackoff
	}
	// Full jitter over (0, backoff]: rand.Int64N returns [0, n), so shift by one.
	return time.Duration(rand.Int64N(int64(backoff)) + 1)
}

// retryAfter reads a Retry-After header in either of its two forms — a delay in
// seconds, or an HTTP-date — and reports the delay it asks for, clamped to
// maxRetryAfter. A missing, unparseable, or already-elapsed value reports false so
// the caller backs off instead.
func (l *limiter) retryAfter(header http.Header) (time.Duration, bool) {
	v := strings.TrimSpace(header.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return min(time.Duration(secs)*time.Second, maxRetryAfter), true
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := when.Sub(l.now()); d > 0 {
			return min(d, maxRetryAfter), true
		}
	}
	return 0, false
}

// pace enforces the global minimum spacing between Notion requests: it delays
// this request until at least interval has elapsed since the previously admitted
// one, and reserves its own slot before releasing the gate — so the spacing holds
// no matter which route or page the requests target, and no matter how many
// callers queue behind it. A non-positive interval disables pacing entirely.
func (l *limiter) pace(ctx context.Context) error {
	if l.interval <= 0 {
		return nil
	}
	l.mu.Lock()
	var wait time.Duration
	now := l.now()
	if !l.lastReq.IsZero() {
		if w := l.interval - now.Sub(l.lastReq); w > 0 {
			wait = w
		}
	}
	l.lastReq = now.Add(wait)
	l.mu.Unlock()

	if wait <= 0 {
		return ctx.Err()
	}
	return l.sleep(ctx, wait)
}

// realSleep is the production sleep seam: it waits out d, or returns early if the
// context is cancelled while waiting — a cancelled run must not be held hostage by
// a long Retry-After.
func realSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
