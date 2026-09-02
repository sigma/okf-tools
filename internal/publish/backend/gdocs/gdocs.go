// Package gdocs is a THROWAWAY PROTOTYPE (sigma/okf-tools#152), not a shipping
// backend. It exists to answer one question: does a Google Docs destination — one
// file with ordered tabs — fit the publishing seam that was drawn around Notion's
// database-of-pages?
//
// It implements all five roles against an in-memory fake of the Docs and Drive
// APIs, so the whole pipeline (graph -> optimize -> transport) can drive it
// offline. Where the seam fights, the code says so in a comment marked FIGHT.
package gdocs

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// maxRequests models the practical ceiling on one documents.batchUpdate: the API
// documents no hard request cap, so this stands in for a self-imposed one.
const maxRequests = 6

// tab is one tab of the fake document. id is server-minted, exactly as a real
// tabId is; title and parent mirror TabProperties.
type tab struct {
	id       string
	title    string
	parentID string
	// body is the tab's rendered content, one entry per inserted block.
	body []string
	// headingIDs maps a hosted anchor name to the read-only headingId the fake
	// server assigns. Crucially it is NOT known at batchUpdate time.
	headingIDs map[publish.AnchorName]string
}

// fakeService is a stand-in for the Docs + Drive APIs: one document holding tabs,
// plus the Drive file's appProperties. It counts round-trips so the prototype can
// report what the shape costs.
type fakeService struct {
	tabs []*tab
	// appProps is the Drive file's appProperties map (the find-or-create key plus
	// whatever provenance we try to persist).
	appProps map[string]string
	// batchUpdates and gets count API round-trips.
	batchUpdates int
	gets         int
	nextTabID    int
	nextHeading  int
}

func (s *fakeService) tabByID(id string) *tab {
	for _, t := range s.tabs {
		if t.id == id {
			return t
		}
	}
	return nil
}

// addTab is the fake's AddDocumentTabRequest: it mints a tabId server-side.
func (s *fakeService) addTab(title, parentID string) *tab {
	s.nextTabID++
	t := &tab{
		id:         fmt.Sprintf("t.%d", s.nextTabID),
		title:      title,
		parentID:   parentID,
		headingIDs: map[publish.AnchorName]string{},
	}
	s.tabs = append(s.tabs, t)
	return t
}

// Backend implements every publishing role against the fake service.
type Backend struct {
	svc *fakeService
	// pathToTab maps a node's bundle-relative path to its tab id, recovered from
	// appProperties at scan time and extended as tabs are minted.
	pathToTab map[string]string
	// overflow records every piece of provenance that did not fit in Drive's
	// appProperties budget — the evidence for the write-back FIGHT.
	overflow []string
}

// Overflow reports the provenance that could not be persisted.
func (b *Backend) Overflow() []string { return b.overflow }

var (
	_ backend.Tokenizer       = (*Backend)(nil)
	_ backend.ConstraintModel = (*Backend)(nil)
	_ backend.Executor        = (*Backend)(nil)
	_ backend.Scanner         = (*Backend)(nil)
	_ backend.WriteBacker     = (*Backend)(nil)
	_ backend.Backend         = (*Backend)(nil)
)

// New builds a prototype backend over a fresh empty document.
func New() *Backend {
	return &Backend{
		svc:       &fakeService{appProps: map[string]string{}},
		pathToTab: map[string]string{},
	}
}

// Stats reports the round-trips the run cost, so the prototype can compare the
// per-Group batching the seam imposes against what one document could have done.
func (b *Backend) Stats() (batchUpdates, gets, tabs int) {
	return b.svc.batchUpdates, b.svc.gets, len(b.svc.tabs)
}

// Dump renders the document for assertions: one line per tab, then its body.
func (b *Backend) Dump() string {
	var sb strings.Builder
	ts := append([]*tab(nil), b.svc.tabs...)
	sort.Slice(ts, func(i, j int) bool { return ts[i].title < ts[j].title })
	for _, t := range ts {
		fmt.Fprintf(&sb, "== tab %s (%s) parent=%q\n", t.title, t.id, t.parentID)
		for _, line := range t.body {
			fmt.Fprintf(&sb, "   %s\n", line)
		}
	}
	return sb.String()
}

// --- payloads (opaque to the pipeline) --------------------------------------

// createTab is the unit for CreateNode: an AddDocumentTabRequest.
type createTab struct{ node publish.SymbolicID }

// setProps is the unit for SetProperties.
//
// FIGHT (properties have no home). A Notion node is a database row with typed
// columns; a Docs tab has exactly three writable properties — title, parentTabId,
// iconEmoji — and none of them holds a node's frontmatter. The prototype renders
// the properties as a leading "key: value" block inside the tab, which means
// SetProperties stops being a distinct backend op and becomes *more content*.
type setProps struct {
	node  publish.SymbolicID
	props map[string]any
}

// deleteTab is the unit for DeleteNode: a DeleteTabRequest.
type deleteTab struct{ node publish.SymbolicID }

// contentBlock is one block of a tab's content.
type contentBlock struct {
	kind    int
	level   int
	runs    []publish.Run
	anchors []publish.AnchorName
}

var _ = context.Background

// Props exposes the Drive appProperties for prototype assertions.
func (b *Backend) Props() map[string]string { return b.svc.appProps }
