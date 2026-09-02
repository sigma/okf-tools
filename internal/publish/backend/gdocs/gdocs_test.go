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
		"CONTEXT.md": "---\nokf_version: \"0.1\"\ntitle: Context\ntype: context\n---\n\n# Glossary\n\n**Widget** — a thing.\n",
		"alpha.md":   "---\nokf_version: \"0.1\"\ntitle: Alpha\ntype: concept\n---\n\n# Alpha\n\nAlpha links to [Beta](/beta.md) and cites a **Widget**.\n",
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

	for _, want := range []string{"index.md", "alpha.md", "beta.md", "CONTEXT.md"} {
		if !containsStr(titles, want) {
			t.Errorf("no tab for %s (got %v)", want, titles)
		}
	}
	if body := fake.tabBody(docID, "alpha.md"); !strings.Contains(body, "Alpha links to") {
		t.Errorf("alpha tab body missing content: %q", body)
	}
	if fake.untabbedWrites != 0 {
		t.Errorf("%d write(s) omitted a tabId and silently hit the first tab", fake.untabbedWrites)
	}
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

func containsStr(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
