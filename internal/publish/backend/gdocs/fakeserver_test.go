package gdocs_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"
)

// fakeGoogle is an in-memory stand-in for the Drive and Docs APIs: enough of both
// to drive a real publish offline, in the same spirit as the Notion backend's
// fake server. It models the behaviours this backend depends on — appProperties
// lookup scoped to a drive, server-minted tab ids, per-tab bodies, and
// tab-scoped content ranges.
type fakeGoogle struct {
	mu sync.Mutex
	t  *testing.T

	files map[string]*fakeFile
	docs  map[string]*fakeDoc
	seq   int

	// batchUpdates counts documents.batchUpdate calls, so a test can assert the
	// write cost rather than infer it.
	batchUpdates int
	// untabbedWrites counts requests that omitted a tabId — the silent
	// first-tab-targeting footgun (#147). It must stay zero.
	untabbedWrites int
}

type fakeFile struct {
	id       string
	name     string
	mimeType string
	parents  []string
	appProps map[string]string
	content  []byte
}

type fakeDoc struct {
	id   string
	tabs []*fakeTab
}

type fakeTab struct {
	id    string
	title string
	body  string
	// headings maps a paragraph start index to the headingId the server assigns.
	// The real API mints these itself and exposes them as READ-ONLY, which is why
	// the backend has to read the document back to learn them.
	headings map[int]string
	// links records every link applied to this tab's text, so a test can assert
	// what a cross-reference actually targeted rather than trusting the text.
	links []map[string]any
	// namedRanges records identity markers by name, so a test can assert they are
	// re-asserted on every rewrite rather than assumed to survive.
	namedRanges map[string]bool
}

func newFakeTab(id, title string) *fakeTab {
	return &fakeTab{id: id, title: title, headings: map[int]string{}, namedRanges: map[string]bool{}}
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	return &fakeGoogle{t: t, files: map[string]*fakeFile{}, docs: map[string]*fakeDoc{}}
}

func (f *fakeGoogle) next(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s%d", prefix, f.seq)
}

func (f *fakeGoogle) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/drive/v3/files", f.driveFiles)
	mux.HandleFunc("/drive/v3/files/", f.driveFile)
	mux.HandleFunc("/upload/drive/v3/files/", f.driveUpload)
	mux.HandleFunc("/v1/documents/", f.docsRoute)
	return httptest.NewServer(mux)
}

// driveFiles handles list (find-by-appProperties) and create.
func (f *fakeGoogle) driveFiles(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.Method == http.MethodPost {
		var in struct {
			Name          string            `json:"name"`
			MimeType      string            `json:"mimeType"`
			Parents       []string          `json:"parents"`
			AppProperties map[string]string `json:"appProperties"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if len(in.Parents) == 0 {
			// A create with no parent would land in the service account's My Drive,
			// which has no storage quota — the 403 the real API returns.
			http.Error(w, `{"error":{"message":"storageQuotaExceeded"}}`, http.StatusForbidden)
			return
		}
		id := f.next("file-")
		file := &fakeFile{id: id, name: in.Name, mimeType: in.MimeType, parents: in.Parents, appProps: in.AppProperties}
		f.files[id] = file
		if in.MimeType == "application/vnd.google-apps.document" {
			f.docs[id] = &fakeDoc{id: id, tabs: []*fakeTab{newFakeTab("t.0", "Tab 1")}}
		}
		writeJSON(w, map[string]any{"id": id, "name": in.Name, "appProperties": in.AppProperties})
		return
	}

	q := r.URL.Query().Get("q")
	key, value := parseAppPropertyQuery(q)
	parent := r.URL.Query().Get("driveId")
	var out []map[string]any
	for _, file := range f.files {
		if file.appProps[key] != value || value == "" {
			continue
		}
		if parent != "" && !contains(file.parents, parent) {
			continue
		}
		out = append(out, map[string]any{"id": file.id, "name": file.name, "appProperties": file.appProps})
	}
	writeJSON(w, map[string]any{"files": out})
}

// driveFile handles media download.
func (f *fakeGoogle) driveFile(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
	file, ok := f.files[id]
	if !ok {
		http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
		return
	}
	w.Write(file.content)
}

// driveUpload handles media upload (the sidecar write).
func (f *fakeGoogle) driveUpload(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := strings.TrimPrefix(r.URL.Path, "/upload/drive/v3/files/")
	file, ok := f.files[id]
	if !ok {
		http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
		return
	}
	body, _ := io.ReadAll(r.Body)
	file.content = body
	writeJSON(w, map[string]any{"id": id})
}

func (f *fakeGoogle) docsRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/documents/")
	if id, ok := strings.CutSuffix(rest, ":batchUpdate"); ok {
		f.batchUpdate(w, r, id)
		return
	}
	f.getDocument(w, r, rest)
}

func (f *fakeGoogle) getDocument(w http.ResponseWriter, r *http.Request, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	doc, ok := f.docs[id]
	if !ok {
		http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
		return
	}
	if r.URL.Query().Get("includeTabsContent") != "true" {
		// Mirrors the real API: without the flag only the first tab is visible.
		f.t.Errorf("documents.get without includeTabsContent=true")
	}
	tabs := make([]map[string]any, 0, len(doc.tabs))
	for _, tab := range doc.tabs {
		content := []map[string]any{{"endIndex": u16len(tab.body) + 2}}
		for start, hid := range tab.headings {
			content = append(content, map[string]any{
				"startIndex": start,
				"endIndex":   start + 1,
				"paragraph": map[string]any{
					"paragraphStyle": map[string]any{"headingId": hid, "namedStyleType": "HEADING_6"},
				},
			})
		}
		tabs = append(tabs, map[string]any{
			"tabProperties": map[string]any{"tabId": tab.id, "title": tab.title},
			"documentTab":   map[string]any{"body": map[string]any{"content": content}},
		})
	}
	writeJSON(w, map[string]any{"documentId": id, "tabs": tabs})
}

func (f *fakeGoogle) batchUpdate(w http.ResponseWriter, r *http.Request, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	doc, ok := f.docs[id]
	if !ok {
		http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
		return
	}
	var in struct {
		Requests []map[string]any `json:"requests"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	f.batchUpdates++

	replies := make([]map[string]any, 0, len(in.Requests))
	for _, req := range in.Requests {
		switch {
		case req["addDocumentTab"] != nil:
			props, _ := req["addDocumentTab"].(map[string]any)["tabProperties"].(map[string]any)
			title, _ := props["title"].(string)
			tab := newFakeTab(f.next("t."), title)
			doc.tabs = append(doc.tabs, tab)
			replies = append(replies, map[string]any{
				"addDocumentTab": map[string]any{"tabProperties": map[string]any{"tabId": tab.id, "title": title}},
			})

		case req["deleteTab"] != nil:
			tabID, _ := req["deleteTab"].(map[string]any)["tabId"].(string)
			kept := doc.tabs[:0]
			for _, tab := range doc.tabs {
				if tab.id != tabID {
					kept = append(kept, tab)
				}
			}
			doc.tabs = kept
			replies = append(replies, map[string]any{})

		case req["updateDocumentTabProperties"] != nil:
			upd, _ := req["updateDocumentTabProperties"].(map[string]any)
			props, _ := upd["tabProperties"].(map[string]any)
			id, _ := props["tabId"].(string)
			title, _ := props["title"].(string)
			for _, tab := range doc.tabs {
				if tab.id == id {
					tab.title = title
				}
			}
			replies = append(replies, map[string]any{})

		case req["updateParagraphStyle"] != nil:
			ups, _ := req["updateParagraphStyle"].(map[string]any)
			rng, _ := ups["range"].(map[string]any)
			style, _ := ups["paragraphStyle"].(map[string]any)
			named, _ := style["namedStyleType"].(string)
			tab := f.tabOf(doc, rng)
			if tab != nil && strings.HasPrefix(named, "HEADING_") {
				start := intOf(rng["startIndex"])
				tab.headings[start] = f.next("h.")
			}
			replies = append(replies, map[string]any{})

		case req["updateTextStyle"] != nil:
			uts, _ := req["updateTextStyle"].(map[string]any)
			rng, _ := uts["range"].(map[string]any)
			style, _ := uts["textStyle"].(map[string]any)
			if link, ok := style["link"].(map[string]any); ok {
				if tab := f.tabOf(doc, rng); tab != nil {
					tab.links = append(tab.links, link)
				}
			}
			replies = append(replies, map[string]any{})

		case req["createNamedRange"] != nil:
			cnr, _ := req["createNamedRange"].(map[string]any)
			name, _ := cnr["name"].(string)
			rng, _ := cnr["range"].(map[string]any)
			if tab := f.tabOf(doc, rng); tab != nil {
				tab.namedRanges[name] = true
			}
			replies = append(replies, map[string]any{})

		case req["deleteNamedRange"] != nil:
			dnr, _ := req["deleteNamedRange"].(map[string]any)
			name, _ := dnr["name"].(string)
			crit, hasCrit := dnr["tabsCriteria"].(map[string]any)
			if !hasCrit {
				// The real API applies this to EVERY tab when tabsCriteria is omitted.
				f.untabbedWrites++
				for _, tab := range doc.tabs {
					delete(tab.namedRanges, name)
				}
			} else {
				ids, _ := crit["tabIds"].([]any)
				for _, raw := range ids {
					id, _ := raw.(string)
					for _, tab := range doc.tabs {
						if tab.id == id {
							delete(tab.namedRanges, name)
						}
					}
				}
			}
			replies = append(replies, map[string]any{})

		case req["deleteContentRange"] != nil:
			rng, _ := req["deleteContentRange"].(map[string]any)["range"].(map[string]any)
			tab := f.tabOf(doc, rng)
			if tab != nil {
				tab.body = ""
				tab.headings = map[int]string{}
				tab.links = nil
			}
			replies = append(replies, map[string]any{})

		case req["insertText"] != nil:
			ins, _ := req["insertText"].(map[string]any)
			loc, _ := ins["endOfSegmentLocation"].(map[string]any)
			if loc == nil {
				loc, _ = ins["location"].(map[string]any)
			}
			text, _ := ins["text"].(string)
			tab := f.tabOf(doc, loc)
			if tab != nil {
				tab.body += text
			}
			replies = append(replies, map[string]any{})

		default:
			replies = append(replies, map[string]any{})
		}
	}
	writeJSON(w, map[string]any{"replies": replies})
}

// tabOf resolves the tab a request targets, recording the case where none was
// named — which the real API resolves to the FIRST tab, silently.
func (f *fakeGoogle) tabOf(doc *fakeDoc, loc map[string]any) *fakeTab {
	id, _ := loc["tabId"].(string)
	if id == "" {
		f.untabbedWrites++
		if len(doc.tabs) > 0 {
			return doc.tabs[0]
		}
		return nil
	}
	for _, tab := range doc.tabs {
		if tab.id == id {
			return tab
		}
	}
	return nil
}

// tabTitles reports the document's tab titles, for assertions.
func (f *fakeGoogle) tabTitles(docID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, tab := range f.docs[docID].tabs {
		out = append(out, tab.title)
	}
	return out
}

func (f *fakeGoogle) tabBody(docID, title string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tab := range f.docs[docID].tabs {
		if tab.title == title {
			return tab.body
		}
	}
	return ""
}

func (f *fakeGoogle) sidecar() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, file := range f.files {
		if file.mimeType == "application/json" {
			return string(file.content)
		}
	}
	return ""
}

// linksOf reports the links applied to a tab's text, by title.
func (f *fakeGoogle) linksOf(docID, title string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tab := range f.docs[docID].tabs {
		if tab.title == title {
			return tab.links
		}
	}
	return nil
}

// tabIDOf reports a tab's id by title.
func (f *fakeGoogle) tabIDOf(docID, title string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tab := range f.docs[docID].tabs {
		if tab.title == title {
			return tab.id
		}
	}
	return ""
}

// namedRangesOf reports a tab's identity markers, for assertions.
func (f *fakeGoogle) namedRangesOf(docID, title string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, tab := range f.docs[docID].tabs {
		if tab.title != title {
			continue
		}
		for name := range tab.namedRanges {
			out = append(out, name)
		}
	}
	return out
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func u16len(s string) int { return len(utf16.Encode([]rune(s))) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// parseAppPropertyQuery pulls the key and value out of
// "appProperties has {key='k' and value='v'} and ...".
func parseAppPropertyQuery(q string) (string, string) {
	key := between(q, "key='", "'")
	value := between(q, "value='", "'")
	return key, value
}

func between(s, open, close string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return ""
	}
	return rest[:j]
}
