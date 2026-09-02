package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
)

// selectionBundle has two content areas plus a file-backed glossary area, which
// is the shape the fan-out rules are written against.
func selectionBundle(t *testing.T) *bundle.Bundle {
	t.Helper()
	files := map[string]string{
		"okf.toml":          "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"areas.json":        `{"concepts":{"directory":"concepts","type":"Concept"},"adr":{"directory":"adr","type":"ADR"},"glossary":{"file":"CONTEXT.md","type":"Context","role":"glossary"}}`,
		"index.md":          "---\nokf_version: \"0.1\"\ntitle: Index\ntype: index\n---\n\n# Index\n",
		"CONTEXT.md":        "---\nokf_version: \"0.1\"\ntitle: Context\ntype: context\n---\n\n# Keys\n\n- **Widget**: a thing.\n",
		"concepts/alpha.md": "---\nokf_version: \"0.1\"\ntitle: Alpha\ntype: concept\n---\n\n# Alpha\n",
		"concepts/beta.md":  "---\nokf_version: \"0.1\"\ntitle: Beta\ntype: concept\n---\n\n# Beta\n",
		"adr/0001.md":       "---\nokf_version: \"0.1\"\ntitle: One\ntype: adr\n---\n\n# One\n",
	}
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

// TestDefaultFansOutPerArea pins the default: one document per area, the glossary
// excluded from the fan-out but appended to every document.
func TestDefaultFansOutPerArea(t *testing.T) {
	b := selectionBundle(t)
	plan, err := ResolveSelections(b, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var names []string
	for _, sel := range plan.Selections {
		names = append(names, sel.Name)
	}
	if len(names) != 2 || names[0] != "adr" || names[1] != "concepts" {
		t.Fatalf("want one document per non-glossary area in sorted order, got %v", names)
	}
	for _, sel := range plan.Selections {
		if !sel.Contains("CONTEXT.md") {
			t.Errorf("%s: the glossary must be appended to every document", sel.Name)
		}
	}
	if plan.Selections[0].Contains("concepts/alpha.md") {
		t.Error("the adr document must not contain a concepts page")
	}
	// index.md belongs to no declared area: reported, not fatal.
	if plan.Unclaimed != 1 {
		t.Errorf("want 1 unclaimed page, got %d", plan.Unclaimed)
	}
}

func TestSelectByAreaAndPath(t *testing.T) {
	b := selectionBundle(t)

	plan, err := ResolveSelections(b, []string{"concepts"})
	if err != nil {
		t.Fatalf("resolve area: %v", err)
	}
	if len(plan.Selections) != 1 || plan.Selections[0].Name != "concepts" {
		t.Fatalf("area selection: %+v", plan.Selections)
	}
	if !plan.Selections[0].Contains("concepts/alpha.md") || plan.Selections[0].Contains("adr/0001.md") {
		t.Error("area selection has the wrong scope")
	}

	// A path that is not an area key still resolves.
	plan, err = ResolveSelections(b, []string{"adr/0001.md"})
	if err != nil {
		t.Fatalf("resolve path: %v", err)
	}
	sel := plan.Selections[0]
	if !sel.Contains("adr/0001.md") || sel.Contains("adr/0002.md") {
		t.Error("a single-file selection must match exactly that file")
	}
	if !sel.Contains("CONTEXT.md") {
		t.Error("a single-page document still gets the glossary tab")
	}
	if sel.Title != "0001" {
		t.Errorf("a file selection's title should be its name: %q", sel.Title)
	}
}

// TestSelectingTheGlossaryDoesNotDuplicateIt guards the one case the
// always-append rule must not fire on.
func TestSelectingTheGlossaryDoesNotDuplicateIt(t *testing.T) {
	b := selectionBundle(t)
	plan, err := ResolveSelections(b, []string{"glossary"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	sel := plan.Selections[0]
	if !sel.Contains("CONTEXT.md") {
		t.Fatal("the glossary selection must contain the glossary")
	}
	if sel.Contains("concepts/alpha.md") {
		t.Error("the glossary selection leaked other pages")
	}
}

// TestSelectionMatchingNothingErrors: an empty publish that silently creates an
// empty document is worse than a failure.
func TestSelectionMatchingNothingErrors(t *testing.T) {
	b := selectionBundle(t)
	if _, err := ResolveSelections(b, []string{"nope"}); err == nil {
		t.Error("a selection matching nothing must be an error")
	}
}

// TestNoAreasRegistryPublishesWholeBundle: with no vocabulary to fan out over,
// the bundle is one document.
func TestNoAreasRegistryPublishesWholeBundle(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"okf.toml": "",
		"index.md": "---\nokf_version: \"0.1\"\ntitle: Index\ntype: index\n---\n\n# Index\n",
		"a.md":     "---\nokf_version: \"0.1\"\ntitle: A\ntype: concept\n---\n\n# A\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
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
	plan, err := ResolveSelections(b, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.Selections) != 1 || plan.Selections[0].Name != "." {
		t.Fatalf("want one whole-bundle selection, got %+v", plan.Selections)
	}
	if !plan.Selections[0].Contains("a.md") || !plan.Selections[0].Contains("index.md") {
		t.Error("the whole-bundle selection must contain every page")
	}
}
