package gdocs_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/publish/pipeline"
)

// longTitle is 63 characters — the length of a real ADR title from the bundle
// that hit this, and squarely in the 57–79 band the issue measured.
const longTitle = "A model is a pinned build input, declared exactly like a binary"

// longestTitle is the 79-character worst case the issue reports, and carries a
// colon — the shape that makes a title need quoting in frontmatter.
const longestTitle = "Capabilities and policy: what a component needs, and how a profile satisfies it"

// longTitleBundle is the ordinary case, not a tail case: a descriptive
// sentence-shaped title is normal in an OKF bundle, and 13 of 40 titled pages in
// the reporting bundle were over the limit (#178).
func longTitleBundle() map[string]string {
	return map[string]string{
		"okf.toml": "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		// Titles are QUOTED: these are prose, and an unquoted colon is not valid
		// YAML — an unquoted one silently loses the title and the fixture then tests
		// nothing.
		"index.md": "---\nokf_version: \"0.1\"\ntitle: \"" + longTitle + "\"\ntype: index\n---\n\n" +
			"# Index\n\nThe root page.\n",
		"CONTEXT.md": "---\nokf_version: \"0.1\"\ntitle: Context\ntype: context\n---\n\n" +
			"# Keys\n\n- **Widget**: a small thing.\n",
		"alpha.md": "---\nokf_version: \"0.1\"\ntitle: \"" + longestTitle + "\"\ntype: concept\n---\n\n" +
			"# Alpha\n\nAlpha cites a [Widget](/CONTEXT.md#widget).\n",
	}
}

// TestOverLongTitlesPublish is the #178 regression: the title came from
// frontmatter and went to the API unbounded, so one over-long title failed its
// whole atomic transaction, and the selection with it.
func TestOverLongTitlesPublish(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	be := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), be, loadBundle(t, longTitleBundle())); err != nil {
		t.Fatalf("a bundle with over-long titles failed to publish: %v", err)
	}

	titles := fake.tabTitles(be.DocumentID())
	if len(titles) == 0 {
		t.Fatal("nothing was published")
	}
	for _, title := range titles {
		if n := len([]rune(title)); n > 50 {
			t.Errorf("tab title is %d characters, over the API's limit: %q", n, title)
		}
	}
}

// TestTruncationReadsAsDeliberate: a hard chop at the boundary looks like a
// corrupted title, so a shortened one ends in an ellipsis and a reader can see
// what happened.
func TestTruncationReadsAsDeliberate(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	be := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), be, loadBundle(t, longTitleBundle())); err != nil {
		t.Fatalf("run: %v", err)
	}

	titles := fake.tabTitles(be.DocumentID())
	var truncated int
	for _, title := range titles {
		if !strings.HasSuffix(title, "…") {
			continue
		}
		truncated++
		// The kept text is the head of the original, so the tab is still
		// recognisable in the document outline.
		kept := strings.TrimSuffix(title, "…")
		if !strings.HasPrefix(longTitle, kept) && !strings.HasPrefix(longestTitle, kept) {
			t.Errorf("a truncated title is not a prefix of any source title: %q", title)
		}
	}
	if truncated != 2 {
		t.Errorf("want both over-long titles marked as truncated, got %d of %v", truncated, titles)
	}
	// A title that fits is untouched — no ellipsis, no shortening.
	if !containsStr(titles, "Context") {
		t.Errorf("a short title was altered: %v", titles)
	}
}
