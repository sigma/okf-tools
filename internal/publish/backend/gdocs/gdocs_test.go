// External test package: it drives the WHOLE pipeline (Provision → Scan →
// Generation → Optimization → Transport → WriteBack) against the Google Docs
// backend, offline, through an httptest stand-in for Drive and Docs. That is the
// same discipline the Notion and filesystem backends are held to.
package gdocs_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish/backend/gdocs"
	"github.com/sigma/okf-tools/internal/publish/pipeline"
)

const testDriveID = "0ADRIVE"

func loadBundle(t *testing.T, files map[string]string) *bundle.Bundle {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, cfgPath, err := bundle.Discover(dir, "", "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	b, err := bundle.Load(root, cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return b
}

func testBundle() map[string]string {
	return map[string]string{
		"okf.toml":   "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md":   "---\nokf_version: \"0.1\"\ntitle: Index\ntype: index\n---\n\n# Index\n\n- [Alpha](/alpha.md)\n- [Beta](/beta.md)\n",
		"CONTEXT.md": "---\nokf_version: \"0.1\"\ntitle: Context\ntype: context\n---\n\n# Keys\n\n- **Widget**: a small thing.\n- **Sprocket**: a toothed thing.\n",
		"alpha.md":   "---\nokf_version: \"0.1\"\ntitle: Alpha\ntype: concept\n---\n\n# Alpha\n\nAlpha links to [Beta](/beta.md) and cites a [Widget](/CONTEXT.md#widget).\n",
		"beta.md":    "---\nokf_version: \"0.1\"\ntitle: Beta\ntype: concept\n---\n\n# Beta\n\nBeta stands alone.\n",
	}
}

// newBackend wires a backend at the fake server with no credentials: the
// transport override bypasses the impersonation path entirely, so the suite
// needs no Google account and touches no network.
func newBackend(t *testing.T, srv string) *gdocs.Backend {
	t.Helper()
	be, err := gdocs.New(context.Background(), gdocs.Config{
		DriveID:       testDriveID,
		Bundle:        "testbundle",
		Selection:     "concepts",
		DocsEndpoint:  srv,
		DriveEndpoint: srv,
		HTTPClient:    &http.Client{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return be
}

func TestPublishCreatesATabPerPage(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	be := newBackend(t, srv.URL)
	b := loadBundle(t, testBundle())

	res, err := pipeline.Run(context.Background(), be, b)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	docID := be.DocumentID()
	if docID == "" {
		t.Fatal("provision did not resolve a document")
	}

	titles := fake.tabTitles(docID)
	t.Logf("tabs: %v", titles)
	t.Logf("txns=%d requests=%d batchUpdates=%d", res.TxnCount, res.Stats.Requests, fake.batchUpdates)

	// Titles come from frontmatter, not the path (#155).
	for _, want := range []string{"Index", "Alpha", "Beta", "Context"} {
		if !containsStr(titles, want) {
			t.Errorf("no tab titled %q (got %v)", want, titles)
		}
	}
	// The document's default tab is ADOPTED by the first page rather than left
	// beside the real content.
	if containsStr(titles, "Tab 1") {
		t.Errorf("the default tab survived into published output: %v", titles)
	}
	if len(titles) != 4 {
		t.Errorf("want one tab per page (4), got %d: %v", len(titles), titles)
	}

	body := fake.tabBody(docID, "Alpha")
	if !strings.Contains(body, "Alpha links to") {
		t.Errorf("alpha tab body missing content: %q", body)
	}
	// A Ref run carries no text of its own, so the backend supplies the label.
	if !strings.Contains(body, "beta") || !strings.Contains(body, "widget") {
		t.Errorf("cross-reference lost its label: %q", body)
	}
	// Frontmatter is rendered as leading content, since a tab has no property surface.
	if !strings.Contains(body, "title: Alpha") {
		t.Errorf("properties not rendered into the tab: %q", body)
	}
	// Every tab carries its identity marker, re-asserted by the write.
	if got := fake.namedRangesOf(docID, "Alpha"); !containsStr(got, "okf:alpha.md") {
		t.Errorf("identity named range missing: %v", got)
	}
	if fake.untabbedWrites != 0 {
		t.Errorf("%d write(s) omitted a tabId and silently hit the first tab", fake.untabbedWrites)
	}
	t.Logf("anchors: %v", res.Anchors)
	assertCrossReferences(t, fake, docID)
	if !res.Metered {
		t.Error("backend did not report RequestStats")
	}
	if res.Stats.Requests == 0 {
		t.Error("RequestStats counted no requests")
	}
	if sc := fake.sidecar(); !strings.Contains(sc, "alpha.md") || !strings.Contains(sc, "propHash") {
		t.Errorf("sidecar missing per-node state: %s", sc)
	}
}

// TestRerunIsANoop is the property the whole change-detection design rests on: a
// second publish of unchanged source must touch nothing.
func TestRerunIsANoop(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	b := loadBundle(t, testBundle())

	first := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), first, b); err != nil {
		t.Fatalf("first run: %v", err)
	}
	docID := first.DocumentID()
	before := fake.batchUpdates
	tabsBefore := len(fake.tabTitles(docID))

	// A fresh backend, as a second CLI invocation would be: everything it knows
	// comes from the drive and the sidecar.
	second := newBackend(t, srv.URL)
	res, err := pipeline.Run(context.Background(), second, b)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	t.Logf("second run: txns=%d batchUpdates %d -> %d", res.TxnCount, before, fake.batchUpdates)
	if res.TxnCount != 0 {
		t.Errorf("unchanged re-run emitted %d transaction(s), want 0", res.TxnCount)
	}
	if fake.batchUpdates != before {
		t.Errorf("unchanged re-run issued %d batchUpdate(s)", fake.batchUpdates-before)
	}
	if got := len(fake.tabTitles(docID)); got != tabsBefore {
		t.Errorf("re-run changed the tab count: %d -> %d", tabsBefore, got)
	}
	if second.DocumentID() != docID {
		t.Errorf("re-run did not find the same document: %s != %s", second.DocumentID(), docID)
	}
}

// TestProvisionIsIdempotent checks the Provisioner contract directly.
func TestProvisionIsIdempotent(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	be := newBackend(t, srv.URL)
	ctx := context.Background()
	if err := be.Provision(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	first := be.DocumentID()

	again := newBackend(t, srv.URL)
	if err := again.Provision(ctx); err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	if again.DocumentID() != first {
		t.Errorf("provision created a second document: %s != %s", again.DocumentID(), first)
	}
}

// assertCrossReferences checks what a reference actually TARGETS, which the
// rendered text cannot show: a page link must address the target tab, and a
// glossary citation must address a heading inside the glossary tab — the
// two-pass write's whole purpose.
func assertCrossReferences(t *testing.T, fake *fakeGoogle, docID string) {
	t.Helper()
	betaTab := fake.tabIDOf(docID, "Beta")
	glossaryTab := fake.tabIDOf(docID, "Context")

	var sawTabLink, sawHeadingLink bool
	for _, link := range fake.linksOf(docID, "Alpha") {
		if id, ok := link["tabId"].(string); ok && id == betaTab {
			sawTabLink = true
		}
		if h, ok := link["heading"].(map[string]any); ok {
			if h["tabId"] == glossaryTab && h["id"] != "" {
				sawHeadingLink = true
			}
		}
	}
	if !sawTabLink {
		t.Errorf("the link to Beta did not target its tab (%s): %v", betaTab, fake.linksOf(docID, "Alpha"))
	}
	if !sawHeadingLink {
		t.Errorf("the Widget citation did not target a heading in the glossary tab (%s): %v",
			glossaryTab, fake.linksOf(docID, "Alpha"))
	}
}

// areaBundle declares two areas plus a file-backed glossary, and gives the
// concepts area a landing README — the page Notion excludes as "not a row".
func areaBundle() map[string]string {
	return map[string]string{
		"okf.toml":   "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"areas.json": `{"concepts":{"directory":"concepts","type":"Concept"},"glossary":{"file":"CONTEXT.md","type":"Context","role":"glossary"}}`,
		"CONTEXT.md": "---\nokf_version: \"0.1\"\ntitle: Context\ntype: context\n---\n\n# Keys\n\n- **Widget**: a thing.\n",
		// Sorts BEFORE README.md, so a correct result cannot come from the path order.
		"concepts/ADR-index.md": "---\nokf_version: \"0.1\"\ntitle: ADR Index\ntype: concept\n---\n\n# ADR Index\n",
		"concepts/README.md":    "---\nokf_version: \"0.1\"\ntitle: Concepts Overview\ntype: concept\n---\n\n# Concepts\n\nWhat this area covers.\n",
		"concepts/alpha.md":     "---\nokf_version: \"0.1\"\ntitle: Alpha\ntype: concept\n---\n\n# Alpha\n",
	}
}

// TestAreaRootOpensTheDocument: the area's landing README is published (Notion
// excludes it) and is the FIRST tab, asserted rather than inherited from the path
// sort — the fixture deliberately contains a page that sorts ahead of README.md.
func TestAreaRootOpensTheDocument(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	b := loadBundle(t, areaBundle())
	be, err := gdocs.New(context.Background(), gdocs.Config{
		DriveID: testDriveID, Bundle: "testbundle", Selection: "concepts",
		DocsEndpoint: srv.URL, DriveEndpoint: srv.URL, HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	plan, err := pipeline.ResolveSelections(b, []string{"concepts"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := pipeline.Run(context.Background(), be, b,
		pipeline.WithSelection(plan.Selections[0].Contains)); err != nil {
		t.Fatalf("run: %v", err)
	}

	titles := fake.tabTitles(be.DocumentID())
	t.Logf("area document tabs: %v", titles)

	if len(titles) == 0 || titles[0] != "Concepts Overview" {
		t.Errorf("the area README must open the document, got %v", titles)
	}
	if !containsStr(titles, "ADR Index") || !containsStr(titles, "Alpha") {
		t.Errorf("the area's other pages are missing: %v", titles)
	}
	// The glossary is appended to every selection.
	if !containsStr(titles, "Context") {
		t.Errorf("the glossary tab is missing: %v", titles)
	}
}

// TestNotionScopeIsUnchanged: a backend that does NOT publish area roots still
// gets the old node set, so this change cannot leak into the Notion mirror.
func TestNotionScopeIsUnchanged(t *testing.T) {
	b := loadBundle(t, areaBundle())
	var found bool
	for _, d := range b.PublishDocs() {
		if d.Rel == "concepts/README.md" {
			found = true
		}
	}
	if found {
		t.Error("bundle.PublishDocs must still exclude an area's landing README")
	}
	var roots []string
	for _, d := range b.AreaRootDocs() {
		roots = append(roots, d.Rel)
	}
	if len(roots) != 1 || roots[0] != "concepts/README.md" {
		t.Errorf("AreaRootDocs should return exactly the area landing README, got %v", roots)
	}
}

// TestSelectionNarrowsTheDocument drives a real fan-out slice end to end: a
// selection publishes only its own pages plus the glossary, and each selection
// gets its OWN document, found by its own identity key.
func TestSelectionNarrowsTheDocument(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	b := loadBundle(t, testBundle())
	be, err := gdocs.New(context.Background(), gdocs.Config{
		DriveID: testDriveID, Bundle: "testbundle", Selection: "alpha-only",
		DocsEndpoint: srv.URL, DriveEndpoint: srv.URL, HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	only := map[string]bool{"alpha.md": true, "CONTEXT.md": true}
	res, err := pipeline.Run(context.Background(), be, b,
		pipeline.WithSelection(func(rel string) bool { return only[rel] }))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	titles := fake.tabTitles(be.DocumentID())
	t.Logf("selection tabs: %v (txns=%d)", titles, res.TxnCount)

	if containsStr(titles, "Beta") || containsStr(titles, "Index") {
		t.Errorf("pages outside the selection were published: %v", titles)
	}
	for _, want := range []string{"Alpha", "Context"} {
		if !containsStr(titles, want) {
			t.Errorf("selection missing %q: %v", want, titles)
		}
	}

	// The link to Beta is DEMOTED, not dropped and not left dangling: its text
	// survives, and it targets no tab (there is no Beta tab in this document).
	body := fake.tabBody(be.DocumentID(), "Alpha")
	if !strings.Contains(body, "Beta") && !strings.Contains(body, "beta") {
		t.Errorf("the out-of-selection reference lost its text: %q", body)
	}
	for _, link := range fake.linksOf(be.DocumentID(), "Alpha") {
		if _, ok := link["tabId"]; ok {
			t.Errorf("a reference outside the selection still targets a tab: %v", link)
		}
	}
	// The glossary citation still resolves, because the glossary is in scope.
	var sawHeading bool
	for _, link := range fake.linksOf(be.DocumentID(), "Alpha") {
		if _, ok := link["heading"]; ok {
			sawHeading = true
		}
	}
	if !sawHeading {
		t.Error("the glossary citation should still resolve inside a narrowed document")
	}
}

func containsStr(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
