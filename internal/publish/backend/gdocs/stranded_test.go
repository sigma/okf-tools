package gdocs_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle/bundletest"
	"github.com/sigma/okf-tools/internal/publish/pipeline"
)

// bigPage is a page too large for the fake's character cap in the stranding
// tests, so its content write is refused after its tab already exists.
const bigPage = "---\nokf_version: \"0.1\"\ntitle: Big\ntype: concept\n---\n\n# Big\n\n"

func withBigPage(n int) map[string]string {
	files := testBundle()
	files["big.md"] = bigPage + strings.Repeat("word ", n) + "\n"
	return files
}

// TestFailedWriteStrandsNoTab is the #216 regression: a content write refused
// after its create left a titled tab with no identity marker, and the next scan
// counted its title as taken.
func TestFailedWriteStrandsNoTab(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	fake.charLimit = 1000
	first := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), first, bundletest.Load(t, withBigPage(400))); err == nil {
		t.Fatal("a write over the document's cap succeeded; the fake did not model the refusal")
	}
	docID := first.DocumentID()
	if containsStr(fake.tabTitles(docID), "Big") {
		t.Errorf("the failed page left its tab behind: %v", fake.tabTitles(docID))
	}

	// With the cap lifted the page publishes under its own title: nothing claims it.
	fake.charLimit = 0
	second := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), second, bundletest.Load(t, withBigPage(400))); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	assertAllMarked(t, fake, docID)
	if !containsStr(fake.tabTitles(docID), "Big") {
		t.Errorf("the page did not get its own title back: %v", fake.tabTitles(docID))
	}
}

// TestEmptyStrayTabsAreAdopted: a run killed between a create and its content
// leaves an empty, unmarked tab no cleanup ran for. The next run reuses it for a
// page rather than leaving it to hold a title forever (#216).
func TestEmptyStrayTabsAreAdopted(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	first := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), first, bundletest.Load(t, testBundle())); err != nil {
		t.Fatalf("first run: %v", err)
	}
	docID := first.DocumentID()
	// The two orphans trunk's document carried: the title, then the
	// directory-prefixed title disambiguate fell back to.
	fake.addStrayTab(docID, "Gamma", "")
	fake.addStrayTab(docID, "notes / Gamma", "")
	before := len(fake.tabTitles(docID))

	files := testBundle()
	files["notes/gamma.md"] = "---\nokf_version: \"0.1\"\ntitle: Gamma\ntype: concept\n---\n\n# Gamma\n\nGamma.\n"
	second := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), second, bundletest.Load(t, files)); err != nil {
		t.Fatalf("a page blocked by stray tabs failed to publish: %v", err)
	}
	if got := len(fake.tabTitles(docID)); got != before {
		t.Errorf("the page added a tab beside the strays instead of adopting one: %d tabs, want %d: %v",
			got, before, fake.tabTitles(docID))
	}
	if !containsStr(fake.tabTitles(docID), "Gamma") {
		t.Errorf("the adopting page did not take its own title: %v", fake.tabTitles(docID))
	}
}

// TestTitleClaimsAreNumberedUntilFree: tabs a human made, with content, are not
// adopted, and hold both the title and its directory-prefixed form. The page is
// numbered instead of failing every run with "Tab title must be unique" (#216).
func TestTitleClaimsAreNumberedUntilFree(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	first := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), first, bundletest.Load(t, testBundle())); err != nil {
		t.Fatalf("first run: %v", err)
	}
	docID := first.DocumentID()
	fake.addStrayTab(docID, "Gamma", "a human's notes\n")
	fake.addStrayTab(docID, "notes / Gamma", "more notes\n")

	files := testBundle()
	files["notes/gamma.md"] = "---\nokf_version: \"0.1\"\ntitle: Gamma\ntype: concept\n---\n\n# Gamma\n\nGamma.\n"
	second := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), second, bundletest.Load(t, files)); err != nil {
		t.Fatalf("a page whose titles are all taken failed to publish: %v", err)
	}
	if !containsStr(fake.tabTitles(docID), "notes / Gamma (2)") {
		t.Errorf("want the page numbered past both claims: %v", fake.tabTitles(docID))
	}
}

// assertAllMarked fails for any tab in the document with no identity marker.
func assertAllMarked(t *testing.T, fake *fakeGoogle, docID string) {
	t.Helper()
	for _, title := range fake.tabTitles(docID) {
		if len(fake.namedRangesOf(docID, title)) == 0 {
			t.Errorf("tab %q carries no identity marker; tabs: %v", title, fake.tabTitles(docID))
		}
	}
}
