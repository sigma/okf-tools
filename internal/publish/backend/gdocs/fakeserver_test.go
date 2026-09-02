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
			f.docs[id] = &fakeDoc{id: id, tabs: []*fakeTab{{id: "t.0", title: "Tab 1"}}}
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
		tabs = append(tabs, map[string]any{
			"tabProperties": map[string]any{"tabId": tab.id, "title": tab.title},
			"documentTab": map[string]any{
				"body": map[string]any{
					"content": []map[string]any{{"endIndex": len([]rune(tab.body)) + 2}},
				},
			},
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
			tab := &fakeTab{id: f.next("t."), title: title}
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

		case req["deleteContentRange"] != nil:
			rng, _ := req["deleteContentRange"].(map[string]any)["range"].(map[string]any)
			tab := f.tabOf(doc, rng)
			if tab != nil {
				tab.body = ""
			}
			replies = append(replies, map[string]any{})

		case req["insertText"] != nil:
			ins, _ := req["insertText"].(map[string]any)
			loc, _ := ins["endOfSegmentLocation"].(map[string]any)
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
