package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// httpDoer is the one method the shared client needs from net/http. Narrowing to
// it lets a test inject an *http.Client whose Transport records requests, or any
// other doer, without the backend importing httptest.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// do performs one JSON request against the shared client and decodes the response
// body into out (nil to discard). It sets the auth, version, and content-type
// headers every Notion call needs, and turns a non-2xx status into an error
// carrying the response body so a failing call is diagnosable. It is the single
// choke point both the Executor and the Scanner trade through, so Notion's HTTP
// specifics never leak past this file.
func (b *Backend) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("notion: marshal %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("notion: build request %s %s: %w", method, path, err)
	}
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	req.Header.Set("Notion-Version", b.notionVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("notion: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("notion: read %s %s response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notion: %s %s: status %d: %s", method, path, resp.StatusCode, string(data))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("notion: decode %s %s response: %w", method, path, err)
		}
	}
	return nil
}

// --- wire types shared by the Executor and Scanner --------------------------

// object is any Notion object carrying at least an id — a created page, an
// appended block. The Executor reads the id back to seed the resolution table.
type object struct {
	ID string `json:"id"`
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
