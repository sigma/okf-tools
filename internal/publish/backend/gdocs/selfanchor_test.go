package gdocs_test

import (
	"context"
	"github.com/sigma/okf-tools/internal/bundle/bundletest"
	"net/http"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/publish/backend/gdocs"
	"github.com/sigma/okf-tools/internal/publish/pipeline"
)

// selfCitingBundle is the normal shape of a glossary: one term's definition
// cites another term defined in the SAME file, so the citation's target is
// hosted by the very tab that carries the citation (#170).
func selfCitingBundle() map[string]string {
	return map[string]string{
		"okf.toml": "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"CONTEXT.md": "---\nokf_version: \"0.1\"\ntitle: Context\ntype: context\n---\n\n" +
			"# Keys\n\n" +
			"- **Counterparty**: the party a [Counterparty-attributed access](/CONTEXT.md#counterparty-attributed-access) is granted to.\n" +
			"- **Counterparty-attributed access**: access attributed to a counterparty.\n",
		"alpha.md": "---\nokf_version: \"0.1\"\ntitle: Alpha\ntype: concept\n---\n\n" +
			"# Alpha\n\nAlpha cites a [Counterparty](/CONTEXT.md#counterparty).\n",
	}
}

// TestGlossarySelfReferenceResolves is the #170 regression: every publish of a
// self-citing glossary failed with "content ref anchor:... did not resolve",
// because the citing transaction IS the transaction that hosts the anchor.
func TestGlossarySelfReferenceResolves(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	be := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), be, bundletest.Load(t, selfCitingBundle())); err != nil {
		t.Fatalf("run: %v", err)
	}

	docID := be.DocumentID()
	links := fake.linksOf(docID, "Context")
	if len(links) == 0 {
		t.Fatal("the glossary's own citation produced no link")
	}
	// The self-citation targets a heading in its OWN tab, by the id the server
	// minted — not a placeholder, and not another tab.
	tabID := fake.tabIDOf(docID, "Context")
	var found bool
	for _, l := range links {
		h, ok := l["heading"].(map[string]any)
		if !ok {
			continue
		}
		if h["tabId"] != tabID {
			t.Errorf("the self-citation points at tab %v, want its own tab %s", h["tabId"], tabID)
		}
		id, _ := h["id"].(string)
		if id == "" || id == gdocs.DryRunHeadingID || strings.Contains(id, gdocs.DeferredHeadingID) {
			t.Errorf("the self-citation kept a placeholder target %q", id)
		}
		if !strings.HasPrefix(id, "h.") {
			t.Errorf("the self-citation target %q is not a harvested headingId", id)
		}
		found = true
	}
	if !found {
		t.Errorf("no heading link on the glossary tab: %v", links)
	}
}

// glossaryOnlyBundle is selfCitingBundle without the page that cites the
// glossary from another tab.
func glossaryOnlyBundle() map[string]string {
	files := selfCitingBundle()
	delete(files, "alpha.md")
	files["index.md"] = "---\nokf_version: \"0.1\"\ntitle: Index\ntype: index\n---\n\n# Index\n\nNo citations here.\n"
	return files
}

// TestDryRunPlansTheSelfReference: --dry-run is where #170 was first seen, so it
// has to plan the deferred link too — a dump missing the patch would show only
// part of the run it promises to show.
func TestDryRunPlansTheSelfReference(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	var dump strings.Builder
	be, err := gdocs.New(context.Background(), gdocs.Config{
		DriveID: testDriveID, Bundle: "testbundle", Selection: "concepts",
		DocsEndpoint: srv.URL, DriveEndpoint: srv.URL, HTTPClient: &http.Client{},
		DryRunWriter: &dump,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// The fixture drops the citing PAGE, so the only anchor link the run can plan
	// is the glossary's citation of itself. With alpha.md present its ordinary
	// cross-tab link carries the same placeholder, and the assertion below would
	// pass even if the deferred patch were never dumped.
	if _, err := pipeline.Run(context.Background(), be, bundletest.Load(t, glossaryOnlyBundle())); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.batchUpdates != 0 {
		t.Errorf("a dry run issued %d batchUpdate(s)", fake.batchUpdates)
	}
	// The planned link must be the SELF-citation — a link to the dry run's own
	// placeholder heading. Asserting only on "link" would pass on Alpha's ordinary
	// cross-tab citation even if the deferred patch were never dumped at all.
	out := dump.String()
	if !strings.Contains(out, gdocs.DryRunHeadingID) {
		t.Errorf("the dump does not plan the deferred self-citation link:\n%s", out)
	}
	if strings.Contains(out, gdocs.DeferredHeadingID) {
		t.Errorf("the dump leaked the deferral sentinel %q", gdocs.DeferredHeadingID)
	}
}

// TestCrossTabAnchorStillResolves guards the case that already worked: a page
// citing a term hosted by a DIFFERENT tab must keep resolving through the base
// resolver, not be captured by the new transaction-local overlay.
func TestCrossTabAnchorStillResolves(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	be := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), be, bundletest.Load(t, selfCitingBundle())); err != nil {
		t.Fatalf("run: %v", err)
	}

	docID := be.DocumentID()
	glossaryTab := fake.tabIDOf(docID, "Context")
	links := fake.linksOf(docID, "Alpha")
	if len(links) == 0 {
		t.Fatal("alpha's citation produced no link")
	}
	for _, l := range links {
		h, ok := l["heading"].(map[string]any)
		if !ok {
			t.Errorf("alpha's glossary citation is not a heading link: %v", l)
			continue
		}
		if h["tabId"] != glossaryTab {
			t.Errorf("alpha cites tab %v, want the glossary tab %s", h["tabId"], glossaryTab)
		}
	}
}
