package notion

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeNotion is a recorded, in-memory stand-in for the Notion API surface the
// Executor and Scanner call. It mints deterministic ids, records every request so a
// test can assert what was written, and serves canned data-source rows so the
// ScanStored path runs offline — no live workspace, deterministic in CI.
type fakeNotion struct {
	mu   sync.Mutex
	seq  int
	reqs []recordedReq

	// children maps a created page id to the block ids minted for its children, so
	// GET /blocks/{id}/children can echo them for anchor mapping.
	children map[string][]string

	// blocks maps a page id to the child blocks the CLIENT actually wrote, each
	// carrying its minted id. Serving these back is what makes a write-then-read
	// round trip honest: a hand-seeded live block can have a shape the executor could
	// never produce, and a scan keyed on that shape passes here while failing against
	// every real mirror (sigma/okf-tools#138).
	//
	// SECOND in the read precedence (after liveBlocks, before the id-only children
	// echo), so a test that seeds canned blocks for a page still gets them.
	blocks map[string][]map[string]any

	// liveBlocks maps a page id to the canned live block objects GET
	// /blocks/{id}/children serves the ScanRecompute walk (full block objects, not
	// just ids), for a page no test actually published.
	//
	// It is FIRST in the precedence a read follows — liveBlocks, then blocks, then
	// children — which is a hazard worth naming: a canned shape outranks what was
	// really written, and a scan keyed on a shape the executor cannot produce then
	// passes here while failing on every live mirror (#138). Prefer publishing
	// through the executor and letting `blocks` answer.
	liveBlocks map[string][]map[string]any

	// pageProps maps a page id to the canned properties GET /pages/{id} serves the
	// write-back read-modify-write (reading a parent row's current `hashes` column).
	pageProps map[string]map[string]any

	// rows are the canned data-source query rows; pageSize > 0 forces pagination.
	rows     []map[string]any
	pageSize int

	// dsProps is the data source's canned existing column set GET /data_sources/{id}
	// serves the Provisioner reconcile — column name → a property object carrying
	// at least a "type". A PATCH /data_sources/{id} merges its added columns in, so
	// a second reconcile in the same test sees them as present.
	dsProps map[string]map[string]any

	// received counts EVERY request the server saw, throttled ones included — the
	// ground truth a run's own reported request count is checked against (#134).
	received int

	// failCreateFrom, when > 0, makes the Nth page create onward fail with a 400 —
	// the fault injection for a run that dies partway (#135).
	failCreateFrom int
	creates        int

	// throttle is how many of the next requests the fake answers with Notion's 429
	// rate-limit reply instead of serving them — the fault injection that exercises
	// the client's retry path end to end. A throttled request never reaches a
	// handler, so it is not recorded: the request log stays the log of what the
	// workspace actually saw.
	throttle int

	// childPages marks the page ids that live as a `child_page` under another page
	// rather than as a row of the data source. Such a page has only a title and none
	// of the data source's column properties, so writing a column property to one is
	// a 400 "Invalid property identifier" — the real Notion behaviour behind
	// sigma/okf-tools#104 and #128. Pages created with a page parent are marked
	// automatically; a test seeds an already-existing subpage directly.
	childPages map[string]bool
}

// recordedReq is one captured request: its method, path, decoded JSON body, and
// the Notion-Version header the client pinned on it.
type recordedReq struct {
	Method  string
	Path    string
	Body    map[string]any
	Version string
}

func newFakeNotion() *fakeNotion {
	return &fakeNotion{
		children:   map[string][]string{},
		blocks:     map[string][]map[string]any{},
		liveBlocks: map[string][]map[string]any{},
		pageProps:  map[string]map[string]any{},
		dsProps:    map[string]map[string]any{},
		childPages: map[string]bool{},
	}
}

func (f *fakeNotion) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /pages", f.createPage)
	mux.HandleFunc("GET /pages/{id}", f.getPage)
	mux.HandleFunc("PATCH /pages/{id}", f.updatePage)
	mux.HandleFunc("GET /blocks/{id}/children", f.getChildren)
	mux.HandleFunc("PATCH /blocks/{id}/children", f.appendChildren)
	mux.HandleFunc("PATCH /blocks/{id}", f.updateBlock)
	mux.HandleFunc("DELETE /blocks/{id}", f.deleteBlock)
	mux.HandleFunc("POST /data_sources/{id}/query", f.query)
	mux.HandleFunc("GET /data_sources/{id}", f.getDataSource)
	mux.HandleFunc("PATCH /data_sources/{id}", f.patchDataSource)
	return f.rateLimit(mux)
}

// rateLimit fronts the fake surface with Notion's rate limiter: while throttle
// requests remain, it replies 429 with a Retry-After and the request never reaches
// the workspace behind it.
func (f *fakeNotion) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.received++
		throttled := f.throttle > 0
		if throttled {
			f.throttle--
		}
		f.mu.Unlock()
		if !throttled {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Retry-After", "1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "error", "status": 429, "code": "rate_limited",
			"message": "You have been rate limited. Please try again in a few minutes.",
		})
	})
}

// getDataSource serves the data source's current column set for the Provisioner
// reconcile: the canned dsProps under a "properties" key.
func (f *fakeNotion) getDataSource(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	id := r.PathValue("id")
	f.mu.Lock()
	props := map[string]any{}
	for name, p := range f.dsProps {
		props[name] = p
	}
	f.mu.Unlock()
	writeJSON(w, map[string]any{"id": id, "properties": props})
}

// patchDataSource records a column-add and merges the added columns into dsProps,
// so a follow-up reconcile in the same test sees them as already present.
func (f *fakeNotion) patchDataSource(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	id := r.PathValue("id")
	if props, ok := body["properties"].(map[string]any); ok {
		f.mu.Lock()
		for name, def := range props {
			d, _ := def.(map[string]any)
			// Record a minimal type so a re-read looks like a real column.
			typ := ""
			for k := range d {
				typ = k
				break
			}
			f.dsProps[name] = map[string]any{"type": typ}
		}
		f.mu.Unlock()
	}
	writeJSON(w, map[string]any{"id": id})
}

// getPage serves the canned properties of a page for the write-back read-modify-
// write of a parent row's subtree map. An unknown page returns empty properties.
func (f *fakeNotion) getPage(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	id := r.PathValue("id")
	f.mu.Lock()
	props := f.pageProps[id]
	f.mu.Unlock()
	if props == nil {
		props = map[string]any{}
	}
	writeJSON(w, map[string]any{"id": id, "properties": props})
}

// record captures a request and returns its decoded body.
func (f *fakeNotion) record(r *http.Request) map[string]any {
	body := decodeBody(r)
	f.mu.Lock()
	f.reqs = append(f.reqs, recordedReq{
		Method:  r.Method,
		Path:    r.URL.Path,
		Body:    body,
		Version: r.Header.Get("Notion-Version"),
	})
	f.mu.Unlock()
	return body
}

func (f *fakeNotion) createPage(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.creates++
	fail := f.failCreateFrom > 0 && f.creates >= f.failCreateFrom
	f.mu.Unlock()
	if fail {
		f.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "error", "status": 400, "code": "validation_error",
			"message": "injected failure",
		})
		return
	}

	body := f.record(r)

	// A page parented under another page is a child_page: it carries a title and
	// nothing else, so any column property in the create 400s exactly as Notion does.
	parent, _ := body["parent"].(map[string]any)
	childPage := parent != nil && parent["type"] == "page_id"
	if rejectColumnProps(w, body, childPage) {
		return
	}

	f.mu.Lock()
	f.seq++
	pageID := fmt.Sprintf("page-%d", f.seq)
	var childIDs []string
	if ch, ok := body["children"].([]any); ok {
		for _, raw := range ch {
			f.seq++
			id := fmt.Sprintf("block-%d", f.seq)
			childIDs = append(childIDs, id)
			f.blocks[pageID] = append(f.blocks[pageID], storedBlock(id, raw))
		}
	}
	f.children[pageID] = childIDs
	// A top-level create is a data-source row, so the next query must return it —
	// with the properties the create carried (its `path`, #135).
	if !childPage {
		props, _ := body["properties"].(map[string]any)
		stored := map[string]any{}
		for name, v := range props {
			stored[name] = writtenProp(v)
		}
		f.rows = append(f.rows, row(pageID, stored))
	}
	if childPage {
		f.childPages[pageID] = true
	}
	f.mu.Unlock()

	writeJSON(w, map[string]any{"id": pageID})
}

func (f *fakeNotion) updatePage(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	id := r.PathValue("id")

	// The same child_page rule as the create path: a page-parented page has no
	// data-source columns, so PATCHing one 400s (sigma/okf-tools#128).
	f.mu.Lock()
	child := f.childPages[id]
	f.mu.Unlock()
	if rejectColumnProps(w, body, child) {
		return
	}

	// PERSIST the write, so a later read observes it. Recording alone would let a
	// read-modify-write test pass against a workspace that never changed — which is
	// exactly the sequence per-group write-back newly depends on (#135): merge a
	// second subpage into a parent row whose map the run itself just wrote.
	f.mu.Lock()
	if archived, _ := body["archived"].(bool); archived {
		f.rows = slices.DeleteFunc(f.rows, func(row map[string]any) bool { return row["id"] == id })
	}
	if props, ok := body["properties"].(map[string]any); ok {
		cur := f.pageProps[id]
		if cur == nil {
			cur = map[string]any{}
			f.pageProps[id] = cur
		}
		for name, v := range props {
			cur[name] = writtenProp(v)
		}
		for _, row := range f.rows {
			if row["id"] != id {
				continue
			}
			rp, _ := row["properties"].(map[string]any)
			if rp == nil {
				rp = map[string]any{}
				row["properties"] = rp
			}
			for name, v := range props {
				rp[name] = writtenProp(v)
			}
		}
	}
	f.mu.Unlock()

	writeJSON(w, map[string]any{"id": id})
}

// writtenProp converts a property value as the CLIENT sent it into the shape the
// server serves back. A client writes rich_text spans as {"text":{"content":…}};
// Notion echoes them with a server-filled plain_text, which is what the scanner and
// the write-back read path both read.
func writtenProp(v any) any {
	prop, ok := v.(map[string]any)
	if !ok {
		return v
	}
	spans, ok := prop["rich_text"].([]any)
	if !ok {
		return v
	}
	var text string
	for _, span := range spans {
		m, _ := span.(map[string]any)
		if t, ok := m["text"].(map[string]any); ok {
			if c, ok := t["content"].(string); ok {
				text += c
			}
		}
		if pt, ok := m["plain_text"].(string); ok {
			text += pt
		}
	}
	return richProp(text)
}

// rejectColumnProps is the child_page property guard both page-write paths share: if
// the target is a page-parented child_page and the write carries any data-source
// column property (anything that is not the `title`), it replies with Notion's real
// 400 — the failure sigma/okf-tools#104 and #128 are about — and reports true so the
// caller stops. Column names are sorted so the reported one is deterministic.
func rejectColumnProps(w http.ResponseWriter, body map[string]any, childPage bool) bool {
	if !childPage {
		return false
	}
	props, ok := body["properties"].(map[string]any)
	if !ok {
		return false
	}
	names := make([]string, 0, len(props))
	for k := range props {
		if k != "title" {
			names = append(names, k)
		}
	}
	if len(names) == 0 {
		return false
	}
	sort.Strings(names)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object":  "error",
		"status":  400,
		"code":    "validation_error",
		"message": fmt.Sprintf("%s is not a property that exists. Invalid property identifier.", names[0]),
	})
	return true
}

func (f *fakeNotion) getChildren(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	id := r.PathValue("id")
	f.mu.Lock()
	live, hasLive := f.liveBlocks[id]
	written := f.blocks[id]
	ids := f.children[id]
	f.mu.Unlock()

	// The ScanRecompute walk needs full block objects (type, child_page, rich_text);
	// when liveBlocks is seeded for a page, serve those. Otherwise fall back to the
	// id-only echo the create/anchor-mapping path relies on.
	if hasLive {
		results := make([]map[string]any, len(live))
		copy(results, live)
		writeJSON(w, map[string]any{"results": results, "has_more": false})
		return
	}

	// Blocks this fake actually received are served as they were written — the round
	// trip a hand-seeded liveBlocks entry cannot stand in for (#138).
	if len(written) > 0 {
		results := make([]map[string]any, len(written))
		copy(results, written)
		writeJSON(w, map[string]any{"results": results, "has_more": false})
		return
	}

	results := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		results = append(results, map[string]any{"id": id})
	}
	writeJSON(w, map[string]any{"results": results, "has_more": false})
}

// storedBlock is one written child block as the server will serve it back: the
// payload the client sent, plus the id the server minted for it.
func storedBlock(id string, raw any) map[string]any {
	out := map[string]any{"id": id}
	if body, ok := raw.(map[string]any); ok {
		for k, v := range body {
			out[k] = v
		}
	}
	return out
}

func (f *fakeNotion) appendChildren(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	pageID := r.PathValue("id")

	f.mu.Lock()
	var results []map[string]any
	if ch, ok := body["children"].([]any); ok {
		for _, raw := range ch {
			f.seq++
			id := fmt.Sprintf("block-%d", f.seq)
			// The appended blocks join the page's child list, so a follow-up GET (or a
			// content replacement's clear) sees the page as the workspace really holds it.
			f.children[pageID] = append(f.children[pageID], id)
			f.blocks[pageID] = append(f.blocks[pageID], storedBlock(id, raw))
			results = append(results, map[string]any{"id": id})
		}
	}
	f.mu.Unlock()

	writeJSON(w, map[string]any{"results": results})
}

// deleteBlock models Notion's DELETE /blocks/{id}: the block leaves its parent's
// child list. It is what a content replacement issues per existing child before
// appending the new body (sigma/okf-tools#130).
func (f *fakeNotion) deleteBlock(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	id := r.PathValue("id")

	f.mu.Lock()
	for page, ids := range f.children {
		kept := make([]string, 0, len(ids))
		for _, c := range ids {
			if c != id {
				kept = append(kept, c)
			}
		}
		f.children[page] = kept
	}
	// The block leaves the workspace, so a later read must not serve it: a content
	// replacement's clear is what mints NEW ids, and a scan that still saw the old
	// ones would never notice a recorded anchor had gone stale (#138).
	for page, blocks := range f.blocks {
		f.blocks[page] = slices.DeleteFunc(blocks, func(b map[string]any) bool { return b["id"] == id })
	}
	f.mu.Unlock()

	writeJSON(w, map[string]any{"id": id, "archived": true})
}

// updateBlock records a PATCH /blocks/{id} in-place block content update — the
// self-hosted-anchor re-materialization the create path issues once anchor block
// ids exist (#89) — and echoes the block id.
func (f *fakeNotion) updateBlock(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	writeJSON(w, map[string]any{"id": r.PathValue("id")})
}

func (f *fakeNotion) query(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)

	start := 0
	if c, _ := body["start_cursor"].(string); c != "" {
		start, _ = strconv.Atoi(c)
	}
	size := len(f.rows)
	if f.pageSize > 0 {
		size = f.pageSize
	}
	end := start + size
	if end > len(f.rows) {
		end = len(f.rows)
	}
	page := f.rows[start:end]

	resp := map[string]any{"results": page, "has_more": end < len(f.rows)}
	if end < len(f.rows) {
		resp["next_cursor"] = strconv.Itoa(end)
	}
	writeJSON(w, resp)
}

// requestsTo returns the recorded requests matching a method and exact path.
func (f *fakeNotion) requestsTo(method, path string) []recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedReq
	for _, req := range f.reqs {
		if req.Method == method && req.Path == path {
			out = append(out, req)
		}
	}
	return out
}

// countPath returns how many recorded requests hit method+path.
func (f *fakeNotion) countPath(method, path string) int {
	return len(f.requestsTo(method, path))
}

// pathIndex returns the position of the first recorded request matching
// method+path, or -1. It exposes request ORDER, which countPath cannot: a content
// replacement must delete the existing children BEFORE it appends the new ones.
func (f *fakeNotion) pathIndex(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.IndexFunc(f.reqs, func(r recordedReq) bool {
		return r.Method == method && r.Path == path
	})
}

// deletedBlockIDs returns the block ids the run issued DELETE /blocks/{id} for, in
// request order.
func (f *fakeNotion) deletedBlockIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.reqs {
		if r.Method == http.MethodDelete && strings.HasPrefix(r.Path, "/blocks/") {
			out = append(out, strings.TrimPrefix(r.Path, "/blocks/"))
		}
	}
	return out
}

// --- request/response helpers -----------------------------------------------

// decodeBody reads and JSON-decodes a request body into a generic map (nil for an
// empty body).
func decodeBody(r *http.Request) map[string]any {
	data, _ := io.ReadAll(r.Body)
	if len(data) == 0 {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return m
}

// writeJSON encodes v as the JSON response body.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- canned data-source row helpers -----------------------------------------

// row builds a canned data-source query row: a page id plus derived-column
// properties.
func row(id string, props map[string]any) map[string]any {
	return map[string]any{"id": id, "properties": props}
}

// richProp models a Notion rich_text property carrying one plain-text span — the
// shape the self-describing derived columns use.
func richProp(s string) map[string]any {
	return map[string]any{
		"type":      "rich_text",
		"rich_text": []any{map[string]any{"plain_text": s}},
	}
}

// paraLive builds a canned live paragraph block: an id and one plain-text run.
func paraLive(id, text string) map[string]any {
	return map[string]any{
		"id":   id,
		"type": "paragraph",
		"paragraph": map[string]any{
			"rich_text": []any{map[string]any{"plain_text": text}},
		},
	}
}

// childPageLive builds a canned live child_page block: a subpage's page id and its
// title (the subpath key the recompute matches against the stored subtree map).
func childPageLive(id, title string) map[string]any {
	return map[string]any{
		"id":         id,
		"type":       "child_page",
		"child_page": map[string]any{"title": title},
	}
}

// mustJSON marshals v or fails the test — a terse helper for building JSON column
// values inline.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
