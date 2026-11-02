package gdocs_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/bundle/bundletest"
	"github.com/sigma/okf-tools/internal/publish/backend/gdocs"
	"github.com/sigma/okf-tools/internal/publish/pipeline"
)

// splitBundle is one area of four pages, each about 600 characters, plus a
// glossary every document carries.
func splitBundle(t *testing.T) *bundle.Bundle {
	page := func(title string) string {
		return "---\nokf_version: \"0.1\"\ntitle: " + title + "\ntype: concept\n---\n\n# " + title + "\n\n" +
			strings.Repeat("filler ", 80) + "\n"
	}
	return bundletest.Load(t, map[string]string{
		"okf.toml": "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"areas.json": `{"specs":{"directory":"specs","type":"Concept"},` +
			`"glossary":{"file":"CONTEXT.md","type":"Context","role":"glossary"}}`,
		"CONTEXT.md":      "---\nokf_version: \"0.1\"\ntitle: Context\ntype: context\n---\n\n# Keys\n\n- **Widget**: a thing.\n",
		"specs/README.md": "---\nokf_version: \"0.1\"\ntitle: Specs\ntype: index\n---\n\n# Specs\n\nThe overview.\n",
		"specs/a.md":      page("Alpha"),
		"specs/b.md":      page("Beta"),
		"specs/c.md":      page("Gamma"),
		"specs/d.md":      page("Delta"),
	})
}

// publishParts publishes every part of the specs area into the fake, as the
// fan-out does, and returns each part's backend.
func publishParts(t *testing.T, srv string, b *bundle.Bundle, budget int) []*gdocs.Backend {
	t.Helper()
	plan, err := pipeline.ResolveSelections(b, []string{"specs"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	parts, err := pipeline.SplitBySize(b, plan.Selections[0], budget)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	var out []*gdocs.Backend
	for _, p := range parts {
		be, err := gdocs.New(context.Background(), gdocs.Config{
			DriveID: testDriveID, Bundle: "testbundle", Selection: p.Name, Part: p.Part, Parts: p.Parts,
			DocsEndpoint: srv, DriveEndpoint: srv, HTTPClient: &http.Client{},
		})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		if _, err := pipeline.Run(context.Background(), be, b, pipeline.WithSelection(p.Contains)); err != nil {
			t.Fatalf("publish %s: %v", p.Label(), err)
		}
		out = append(out, be)
	}
	return out
}

// TestAreaOverBudgetSplitsAcrossDocuments is #216's first ask: an area over the
// per-document budget publishes as numbered documents, filled in page order,
// never splitting a page, each carrying the glossary.
func TestAreaOverBudgetSplitsAcrossDocuments(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	// Room for the glossary and two pages a part, not three.
	publishParts(t, srv.URL, splitBundle(t), 1600)

	// The glossary sits where its path sorts, as in an unsplit document.
	want := map[string][]string{
		"testbundle/specs#1": {"Specs", "Context", "Alpha", "Beta"},
		"testbundle/specs#2": {"Context", "Gamma", "Delta"},
	}
	for key, tabs := range want {
		id, name := fake.docByKey(key)
		if id == "" {
			t.Fatalf("no document for %s", key)
		}
		part := strings.TrimPrefix(key, "testbundle/specs#")
		if !strings.HasSuffix(name, "("+part+"/2)") {
			t.Errorf("document %s is named %q, want it numbered %s/2", key, name, part)
		}
		if got := fake.tabTitles(id); strings.Join(got, ",") != strings.Join(tabs, ",") {
			t.Errorf("%s holds %v, want %v", key, got, tabs)
		}
		assertAllMarked(t, fake, id)
	}
	if id, _ := fake.docByKey("testbundle/specs"); id != "" {
		t.Error("a split area also created an unsplit document")
	}
}

// TestSplitChangesAreNoticed: each part is a NotebookLM source, so a run that
// changes how many there are says what to add and what to remove (#216).
func TestSplitChangesAreNoticed(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()
	b := splitBundle(t)

	whole := publishParts(t, srv.URL, b, 1_000_000)
	if len(whole) != 1 || len(whole[0].Notices()) != 0 {
		t.Fatalf("an area that fits must publish as one quiet document")
	}
	oldID, _ := fake.docByKey("testbundle/specs")

	split := publishParts(t, srv.URL, b, 1600)
	first := strings.Join(split[0].Notices(), "\n")
	if !strings.Contains(first, oldID) || !strings.Contains(first, "no longer published to") {
		t.Errorf("part 1 did not report the superseded unsplit document %s: %q", oldID, first)
	}
	if !strings.Contains(first, "created document") {
		t.Errorf("part 1 did not report its new document: %q", first)
	}

	again := publishParts(t, srv.URL, b, 1_000_000)
	got := strings.Join(again[0].Notices(), "\n")
	for _, key := range []string{"testbundle/specs#1", "testbundle/specs#2"} {
		id, _ := fake.docByKey(key)
		if !strings.Contains(got, id) {
			t.Errorf("merging back did not report the superseded part %s (%s): %q", key, id, got)
		}
	}
}
