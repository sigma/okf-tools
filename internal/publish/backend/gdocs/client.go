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
)

// Default API endpoints. They are fields on the client rather than constants so
// the test suite can point the whole backend at an httptest server and stay
// offline, exactly as the Notion backend's suite does.
const (
	DefaultDocsEndpoint  = "https://docs.googleapis.com"
	DefaultDriveEndpoint = "https://www.googleapis.com"
	DefaultIAMEndpoint   = "https://iamcredentials.googleapis.com"
)

// client is the thin REST transport shared by every role. It counts attempts so
// the backend can satisfy RequestReporter, and retries the two failures Google
// asks callers to retry.
type client struct {
	http  *http.Client
	docs  string
	drive string

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

	backoff := 250 * time.Millisecond
	for attempt := 0; ; attempt++ {
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rdr)
		if err != nil {
			return err
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.http.Do(req)
		c.count(func(s *publish.RequestStats) { s.Requests++ })
		if err != nil {
			return fmt.Errorf("gdocs: %s %s: %w", method, url, err)
		}

		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			c.count(func(s *publish.RequestStats) { s.Throttled++ })
		case resp.StatusCode >= 500:
			c.count(func(s *publish.RequestStats) { s.Transient++ })
		default:
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("gdocs: %s %s: %w", method, url, err)
			}
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				return apiError(method, url, resp.StatusCode, raw)
			}
			if out == nil || len(raw) == 0 {
				return nil
			}
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("gdocs: %s %s: decode: %w", method, url, err)
			}
			return nil
		}
		resp.Body.Close()

		if attempt >= 4 {
			return fmt.Errorf("gdocs: %s %s: giving up after %d attempts (last status %d)",
				method, url, attempt+1, resp.StatusCode)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
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
		hint = " (check the service account is a member of the shared drive)"
	}
	return fmt.Errorf("gdocs: %s %s: %d %s%s", method, url, code, msg, hint)
}

// --- Drive ------------------------------------------------------------------

// driveFile is the subset of a Drive file this backend reads.
type driveFile struct {
	ID            string            `json:"id,omitempty"`
	Name          string            `json:"name,omitempty"`
	MimeType      string            `json:"mimeType,omitempty"`
	Parents       []string          `json:"parents,omitempty"`
	AppProperties map[string]string `json:"appProperties,omitempty"`
}

const (
	mimeDoc  = "application/vnd.google-apps.document"
	mimeJSON = "application/json"
)

// findByAppProperty locates a file in one shared drive by a private
// appProperties pair. `in parents` matches DIRECT children only, so a file a
// human drags into a subfolder becomes invisible and would be re-created —
// noted here because it is the mechanism's one sharp edge (#149).
func (c *client) findByAppProperty(ctx context.Context, driveID, key, value string) (*driveFile, error) {
	q := fmt.Sprintf("appProperties has {key='%s' and value='%s'} and '%s' in parents and trashed = false",
		key, value, driveID)
	u := fmt.Sprintf("%s/drive/v3/files?%s", c.drive, url.Values{
		"q":                         {q},
		"corpora":                   {"drive"},
		"driveId":                   {driveID},
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
	u := fmt.Sprintf("%s/v1/documents/%s:batchUpdate", c.docs, id)
	var out struct {
		Replies []map[string]any `json:"replies"`
	}
	if err := c.do(ctx, http.MethodPost, u, map[string]any{"requests": requests}, &out); err != nil {
		return nil, err
	}
	return out.Replies, nil
}
