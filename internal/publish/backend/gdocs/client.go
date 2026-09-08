package gdocs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend/request"
)

// Default API endpoints. They are fields on the client rather than constants so
// the test suite can point the whole backend at an httptest server and stay
// offline, exactly as the Notion backend's suite does.
const (
	DefaultDocsEndpoint  = "https://docs.googleapis.com"
	DefaultDriveEndpoint = "https://www.googleapis.com"
	DefaultIAMEndpoint   = "https://iamcredentials.googleapis.com"
)

// DefaultRequestTimeout bounds ONE attempt's round trip. The budget and the
// reasoning behind it are shared with every other HTTP backend; see the request
// package.
const DefaultRequestTimeout = request.DefaultTimeout

// client is the thin REST transport shared by every role. It counts attempts so
// the backend can satisfy RequestReporter, and retries the two failures Google
// asks callers to retry.
type client struct {
	http  *http.Client
	docs  string
	drive string
	// timeout bounds one attempt's round trip. Non-positive disables the bound,
	// which is the unbounded behaviour #184 is about.
	timeout time.Duration
	// dry, when set, makes every WRITE dump its payload here instead of issuing
	// it. Reads still happen: a dry run against a live document should diff against
	// what is really there, and reading mutates nothing.
	dry io.Writer
	// synth counts synthetic ids handed back for writes that never happened, so a
	// dry run's request stream stays internally consistent.
	synth int
	// now/sleep are the clock seam, real time by default and overridable so a test
	// exercises the retry path with no wall-clock delay — the same seam the Notion
	// client has, and the reason its retry behaviour is testable and this one's was
	// not. A nil now means time.Now; sleep is set at construction.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	mu    sync.Mutex
	stats publish.RequestStats
}

// do issues one JSON request, retrying 429 and 5xx with exponential backoff. A
// nil body sends no payload; out may be nil to discard the response.
func (c *client) do(ctx context.Context, method, url string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}

	for n := 0; ; n++ {
		status, header, done, err := c.attempt(ctx, method, url, payload, out)
		if done {
			return err
		}
		if n >= 4 {
			return fmt.Errorf("gdocs: %s %s: giving up after %d attempts (last status %d)",
				method, url, n+1, status)
		}
		// The shared policy honours a server-sent Retry-After and jitters the
		// backoff. This client did neither: it ignored Retry-After and doubled a bare
		// 250ms, so a throttled fleet re-collided in lockstep and a server asking for
		// a specific pause was overruled.
		if err := c.pause(ctx, request.Delay(header, n+1, c.now)); err != nil {
			return fmt.Errorf("gdocs: %s %s: status %d, retry aborted: %w", method, url, status, err)
		}
	}
}

// attempt performs one round trip. done reports that the call is FINISHED —
// successfully, or with a failure the retry loop must not re-send — and err
// carries its outcome; done false is a 429 or 5xx the caller may back off and
// retry, with status naming which.
//
// It is a function rather than the loop's body so the attempt's deadline is
// released by an ordinary defer on every path, including the ones added later
// (#184).
func (c *client) attempt(ctx context.Context, method, url string, payload []byte, out any) (status int, header http.Header, done bool, err error) {
	// The deadline is per ATTEMPT, so a retry gets its own full budget. A stall is
	// NOT itself made retryable here, and that is a conservative choice rather than
	// a safety argument: this client already replays every route on 429/5xx,
	// including a batchUpdate that may have landed, and #184 is about bounding a
	// hang — widening what gets replayed is a separate call, taken deliberately or
	// not at all. Bounded, a stall fails the run with a diagnosable error instead
	// of hanging it forever.
	attemptCtx, cancel := c.withTimeout(ctx)
	defer cancel()

	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(attemptCtx, method, url, rdr)
	if err != nil {
		return 0, nil, true, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	c.count(func(s *publish.RequestStats) { s.Requests++ })
	if err != nil {
		return 0, nil, true, c.wrapErr(ctx, method, url, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		c.count(func(s *publish.RequestStats) { s.Throttled++ })
		return resp.StatusCode, resp.Header, false, nil
	case resp.StatusCode >= 500:
		c.count(func(s *publish.RequestStats) { s.Transient++ })
		return resp.StatusCode, resp.Header, false, nil
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		// A response whose body never finishes streaming hangs exactly as a missing
		// response does, so it is named the same way.
		return resp.StatusCode, resp.Header, true, c.wrapErr(ctx, "read "+method, url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, resp.Header, true, apiError(method, url, resp.StatusCode, raw)
	}
	if out == nil || len(raw) == 0 {
		return resp.StatusCode, resp.Header, true, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.StatusCode, resp.Header, true, fmt.Errorf("gdocs: %s %s: decode: %w", method, url, err)
	}
	return resp.StatusCode, resp.Header, true, nil
}

// wrapErr names a failed round trip. A stall is reported as a TIMEOUT naming the
// budget it exceeded rather than as a bare "context deadline exceeded", because
// the two read very differently to whoever finds the failed run: one says the
// destination stopped answering, the other looks like a bug in this program.
//
// caller is the UNDERIVED context, so this client's own deadline can be told
// apart from the caller giving up.
func (c *client) wrapErr(caller context.Context, method, url string, err error) error {
	return request.WrapErr(caller, "gdocs", method, url, c.timeout, err)
}

// withTimeout bounds one attempt. It always returns a cancel function, so the
// caller releases the context whether or not a deadline was set.
func (c *client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return request.WithTimeout(ctx, c.timeout)
}

// pause waits out a retry delay through the clock seam, defaulting to real time.
func (c *client) pause(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	return request.Sleep(ctx, d)
}

func (c *client) count(f func(*publish.RequestStats)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(&c.stats)
}

// RequestStats reports what the run cost in API traffic. It is safe to call
// while requests are in flight.
func (c *client) RequestStats() publish.RequestStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// apiError turns a Google error envelope into a diagnosable message, naming the
// two failures whose stock text points nowhere near their cause.
func apiError(method, url string, code int, raw []byte) error {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	msg := env.Error.Message
	if msg == "" {
		msg = string(bytes.TrimSpace(raw))
	}
	hint := ""
	switch {
	case code == http.StatusForbidden && bytes.Contains(raw, []byte("storageQuotaExceeded")):
		hint = " (the destination is not a shared drive: a service account has no storage " +
			"quota and cannot own files, so a My Drive folder cannot work)"
	case code == http.StatusForbidden && bytes.Contains(raw, []byte("ACCESS_TOKEN_SCOPE_INSUFFICIENT")):
		hint = " (the token lacks the Drive/Docs scopes; see gdocs.Scopes)"
	case code == http.StatusNotFound:
		hint = " (check the service account can see the destination: Content manager on " +
			"the folder, or membership of the shared drive)"
	}
	return fmt.Errorf("gdocs: %s %s: %d %s%s", method, url, code, msg, hint)
}

// dumpBatch renders the requests a run WOULD issue and returns plausible replies,
// so the caller's own bookkeeping (a minted tabId, say) stays consistent for the
// rest of the dry run.
func (c *client) dumpBatch(docID string, requests []map[string]any) ([]map[string]any, error) {
	payload, err := json.MarshalIndent(map[string]any{
		"documentId": docID,
		"requests":   requests,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	if _, err := c.dry.Write(append(payload, '\n')); err != nil {
		return nil, err
	}
	replies := make([]map[string]any, 0, len(requests))
	for _, req := range requests {
		if _, ok := req["addDocumentTab"]; ok {
			c.synth++
			replies = append(replies, map[string]any{"addDocumentTab": map[string]any{
				"tabProperties": map[string]any{"tabId": fmt.Sprintf("would-create-t.%d", c.synth)},
			}})
			continue
		}
		replies = append(replies, map[string]any{})
	}
	return replies, nil
}

// --- Drive ------------------------------------------------------------------

// driveFile is the subset of a Drive file this backend reads.
type driveFile struct {
	ID            string            `json:"id,omitempty"`
	DriveID       string            `json:"driveId,omitempty"`
	Name          string            `json:"name,omitempty"`
	MimeType      string            `json:"mimeType,omitempty"`
	Parents       []string          `json:"parents,omitempty"`
	AppProperties map[string]string `json:"appProperties,omitempty"`
}

const (
	mimeDoc    = "application/vnd.google-apps.document"
	mimeJSON   = "application/json"
	mimeFolder = "application/vnd.google-apps.folder"
)

// driveLocation is the destination, split into the two things Drive treats as
// different and the configured id conflated (#168):
//
//   - drive is the search CORPUS, the driveId query parameter, which accepts a
//     shared drive id and nothing else — a folder id is rejected as malformed,
//     404 "Shared drive not found", whatever the caller has been granted.
//   - parent is the WRITE target, which accepts any folder.
//
// For a shared drive root the two coincide, because a drive's id is also its
// root folder's id. That coincidence is why one field worked at all, and why it
// only ever worked there.
type driveLocation struct {
	drive  string
	parent string
}

// String names the destination for a human: a drive root is a DRIVE, and only a
// folder is reported as one — the two are the same id there, and printing
// "folder X (drive X)" would read as a mistake.
func (l driveLocation) String() string {
	if l.parent == l.drive {
		return "drive " + l.drive
	}
	return fmt.Sprintf("folder %s (drive %s)", l.parent, l.drive)
}

// resolveLocation turns the configured id into a corpus and a parent with one
// files.get, so publishing into a folder needs no new configuration.
//
// A shared drive root reports its own id as driveId, which collapses both cases
// into this single path. The drives.get fallback exists because that behaviour
// is documented only by observation: if a root ever answers without a driveId,
// a successful drives.get says "this id IS a drive" and the corpus is itself.
func (c *client) resolveLocation(ctx context.Context, id string) (driveLocation, error) {
	u := fmt.Sprintf("%s/drive/v3/files/%s?supportsAllDrives=true&fields=id,driveId,mimeType", c.drive, id)
	var f driveFile
	if err := c.do(ctx, http.MethodGet, u, nil, &f); err != nil {
		return driveLocation{}, err
	}
	// An absent mimeType is not a rejection: the field is requested, so it is only
	// missing when Drive declines to describe the id, and the write that follows
	// gives a better error than a guess here would.
	if f.MimeType != "" && f.MimeType != mimeFolder {
		return driveLocation{}, fmt.Errorf(
			"gdocs: %s is a %s, not a folder: GDRIVE_FOLDER_ID must name a shared drive or a folder inside one",
			id, f.MimeType)
	}
	if f.DriveID != "" {
		return driveLocation{drive: f.DriveID, parent: id}, nil
	}
	if err := c.assertSharedDrive(ctx, id); err != nil {
		return driveLocation{}, fmt.Errorf(
			"gdocs: %s reports no shared drive, so it is not in one: %w", id, err)
	}
	return driveLocation{drive: id, parent: id}, nil
}

// assertSharedDrive succeeds only if the id names a shared drive, which is the
// question drives.get answers by existing.
func (c *client) assertSharedDrive(ctx context.Context, id string) error {
	u := fmt.Sprintf("%s/drive/v3/drives/%s?fields=id", c.drive, id)
	return c.do(ctx, http.MethodGet, u, nil, nil)
}

// findByAppProperty locates a file under loc.parent by a private appProperties
// pair, searching the shared drive loc.parent lives in.
//
// The corpus and the parent filter are separate on purpose: driveId only accepts
// a drive id, `in parents` accepts a folder (#168). `in parents` also matches
// DIRECT children only, so a file a human drags into a subfolder becomes
// invisible and would be re-created — a sharp edge (#149) that a folder
// destination turns into a supported layout rather than a trap.
func (c *client) findByAppProperty(ctx context.Context, loc driveLocation, key, value string) (*driveFile, error) {
	q := fmt.Sprintf("appProperties has {key='%s' and value='%s'} and '%s' in parents and trashed = false",
		key, value, loc.parent)
	u := fmt.Sprintf("%s/drive/v3/files?%s", c.drive, url.Values{
		"q":                         {q},
		"corpora":                   {"drive"},
		"driveId":                   {loc.drive},
		"includeItemsFromAllDrives": {"true"},
		"supportsAllDrives":         {"true"},
		"fields":                    {"files(id,name,appProperties)"},
	}.Encode())

	var out struct {
		Files []driveFile `json:"files"`
	}
	if err := c.do(ctx, http.MethodGet, u, nil, &out); err != nil {
		return nil, err
	}
	if len(out.Files) == 0 {
		return nil, nil
	}
	return &out.Files[0], nil
}

// createFile creates a file in the shared drive. supportsAllDrives is required
// on EVERY shared-drive call, not just this one.
func (c *client) createFile(ctx context.Context, f driveFile) (*driveFile, error) {
	if c.dry != nil {
		c.synth++
		id := fmt.Sprintf("would-create-file-%d", c.synth)
		fmt.Fprintf(c.dry, "would create %s %q in %v\n", f.MimeType, f.Name, f.Parents)
		return &driveFile{ID: id, Name: f.Name, AppProperties: f.AppProperties}, nil
	}
	u := fmt.Sprintf("%s/drive/v3/files?supportsAllDrives=true&fields=id,name,appProperties", c.drive)
	var out driveFile
	if err := c.do(ctx, http.MethodPost, u, f, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// downloadFile fetches a file's raw content (the sidecar state).
func (c *client) downloadFile(ctx context.Context, id string) ([]byte, error) {
	u := fmt.Sprintf("%s/drive/v3/files/%s?alt=media&supportsAllDrives=true", c.drive, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	c.count(func(s *publish.RequestStats) { s.Requests++ })
	if err != nil {
		return nil, fmt.Errorf("gdocs: download %s: %w", id, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, apiError(http.MethodGet, u, resp.StatusCode, raw)
	}
	return raw, nil
}

// uploadFile replaces a file's content via the media upload endpoint.
func (c *client) uploadFile(ctx context.Context, id string, content []byte) error {
	if c.dry != nil {
		fmt.Fprintf(c.dry, "would write %d byte(s) of state to %s\n", len(content), id)
		return nil
	}
	u := fmt.Sprintf("%s/upload/drive/v3/files/%s?uploadType=media&supportsAllDrives=true", c.drive, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(content))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mimeJSON)
	resp, err := c.http.Do(req)
	c.count(func(s *publish.RequestStats) { s.Requests++ })
	if err != nil {
		return fmt.Errorf("gdocs: upload %s: %w", id, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiError(http.MethodPatch, u, resp.StatusCode, raw)
	}
	return nil
}

// --- Docs -------------------------------------------------------------------

// tabProperties mirrors the writable half of TabProperties. parentTabId is
// present for completeness but never set: tabs are flat (#155).
type tabProperties struct {
	TabID        string `json:"tabId,omitempty"`
	Title        string `json:"title,omitempty"`
	ParentTabID  string `json:"parentTabId,omitempty"`
	Index        *int   `json:"index,omitempty"`
	NestingLevel int    `json:"nestingLevel,omitempty"`
}

type documentTab struct {
	TabProperties tabProperties `json:"tabProperties"`
	DocumentTab   struct {
		Body struct {
			Content []structuralElement `json:"content"`
		} `json:"body"`
		// NamedRanges is keyed by range NAME, which is all this backend needs: the
		// identity marker either exists on this tab or it does not (#176). The
		// values carry each range's spans and are deliberately not decoded.
		NamedRanges map[string]json.RawMessage `json:"namedRanges,omitempty"`
	} `json:"documentTab"`
	ChildTabs []documentTab `json:"childTabs,omitempty"`
}

// structuralElement is one element of a tab's body. Only the paragraph style is
// read: headingId is the anchor target the second pass harvests, and it is
// output-only, which is the whole reason that pass exists (#150).
type structuralElement struct {
	StartIndex int `json:"startIndex"`
	EndIndex   int `json:"endIndex"`
	Paragraph  *struct {
		ParagraphStyle struct {
			HeadingID      string `json:"headingId"`
			NamedStyleType string `json:"namedStyleType"`
		} `json:"paragraphStyle"`
	} `json:"paragraph,omitempty"`
}

type document struct {
	DocumentID string        `json:"documentId"`
	Tabs       []documentTab `json:"tabs"`
}

// getDocument reads the whole document INCLUDING every tab's content.
//
// includeTabsContent is not optional: without it document.body holds only the
// FIRST tab, and the others are invisible rather than empty (#147). It is also
// why ScanStored and ScanRecompute collapse into one operation here — a single
// read returns everything a live recompute would (#152).
func (c *client) getDocument(ctx context.Context, id string) (*document, error) {
	u := fmt.Sprintf("%s/v1/documents/%s?includeTabsContent=true", c.docs, id)
	var doc document
	if err := c.do(ctx, http.MethodGet, u, nil, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// batchUpdate applies an ordered request array. The whole batch is atomic: if
// any request is invalid, nothing is applied.
func (c *client) batchUpdate(ctx context.Context, id string, requests []map[string]any) ([]map[string]any, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	if c.dry != nil {
		return c.dumpBatch(id, requests)
	}
	u := fmt.Sprintf("%s/v1/documents/%s:batchUpdate", c.docs, id)
	var out struct {
		Replies []map[string]any `json:"replies"`
	}
	if err := c.do(ctx, http.MethodPost, u, map[string]any{"requests": requests}, &out); err != nil {
		return nil, err
	}
	return out.Replies, nil
}
