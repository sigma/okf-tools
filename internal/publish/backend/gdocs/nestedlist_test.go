package gdocs_test

import (
	"context"
	"github.com/sigma/okf-tools/internal/bundle/bundletest"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/publish/pipeline"
)

// nestedListBundle puts list items nested under another item on BOTH a content
// page and the glossary — the construct #182 started emitting as real blocks and
// #185 then failed on. The glossary's terms sit AFTER its nested list, so their
// heading offsets only survive if the renderer accounts for the tabs
// createParagraphBullets strips.
func nestedListBundle() map[string]string {
	return map[string]string{
		"okf.toml": "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"CONTEXT.md": "---\nokf_version: \"0.1\"\ntitle: Context\ntype: context\n---\n\n" +
			"# Keys\n\nHow to read this:\n\n" +
			"- first\n  - nested one\n  - nested two\n    - nested three\n- second\n  - nested four\n\n" +
			"- **Widget**: a small thing.\n- **Sprocket**: a toothed thing.\n",
		"alpha.md": "---\nokf_version: \"0.1\"\ntitle: Alpha\ntype: concept\n---\n\n" +
			"# Alpha\n\n" +
			"- top one\n  - nested a\n  - nested b\n    - nested c\n- top two\n  - nested d\n\n" +
			"Alpha cites a [Widget](/CONTEXT.md#widget) after the list.\n",
	}
}

// TestNestedListsPublish is #185: createParagraphBullets DELETES the leading tabs
// it counts, so every index later in the same atomic batch addresses a shorter
// body than the renderer measured. The server rejected the overshoot outright,
// which made three of five selections unpublishable — deterministically, so
// retrying never cleared it.
func TestNestedListsPublish(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	be := newBackend(t, srv.URL)
	b := bundletest.Load(t, nestedListBundle())

	if _, err := pipeline.Run(context.Background(), be, b); err != nil {
		t.Fatalf("run: %v", err)
	}
	docID := be.DocumentID()

	body := fake.tabBody(docID, "Alpha")
	if !strings.Contains(body, "nested c") {
		t.Fatalf("nested content missing: %q", body)
	}
	// The depth tabs are the server's to consume: what lands in the document is
	// bulleted text, not text with tabs in it.
	if strings.Contains(body, "\t") {
		t.Errorf("leading tabs survived into the published body: %q", body)
	}

	// A link whose target is right but whose span slipped is still a broken
	// citation, and the slip is exactly what the strip causes.
	linked := fake.linkedTexts(docID, "Alpha")
	link, ok := linked["widget"]
	if !ok {
		t.Fatalf("the Widget citation does not cover its own label: %v", linked)
	}
	glossaryTab := fake.tabIDOf(docID, "Context")
	h, ok := link["heading"].(map[string]any)
	if !ok || h["tabId"] != glossaryTab {
		t.Fatalf("the citation did not target the glossary tab %s: %v", glossaryTab, link)
	}
	// The harvested heading id has to be one the glossary tab really carries: the
	// harvest matches paragraph starts, which the strip moves.
	id, ok := h["id"].(string)
	if ids := fake.headingIDsOfTab(docID, glossaryTab); !ok || !ids[id] {
		t.Errorf("citation targets heading %v, which the glossary tab does not carry: %v", h["id"], ids)
	}
}

// TestNestedListsRepublish covers the report's own reproduction: publish, then
// publish again over the content the first run left behind. The second pass has
// to converge on the same geometry as the first — the report's destination could
// not converge at all, since retrying a deterministic 400 changes nothing.
func TestNestedListsRepublish(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	b := bundletest.Load(t, nestedListBundle())
	for _, pass := range []string{"first", "second"} {
		be := newBackend(t, srv.URL)
		if _, err := pipeline.Run(context.Background(), be, b, pipeline.WithForceRewrite()); err != nil {
			t.Fatalf("%s run: %v", pass, err)
		}
		docID := be.DocumentID()
		if body := fake.tabBody(docID, "Alpha"); strings.Contains(body, "\t") {
			t.Errorf("%s pass left depth tabs in the body: %q", pass, body)
		}
		if _, ok := fake.linkedTexts(docID, "Alpha")["widget"]; !ok {
			t.Errorf("%s pass: the citation slipped off its label: %v",
				pass, fake.linkedTexts(docID, "Alpha"))
		}
		if got := fake.namedRangesOf(docID, "Alpha"); !containsStr(got, "okf:alpha.md") {
			t.Errorf("%s pass: identity marker missing: %v", pass, got)
		}
	}
}
