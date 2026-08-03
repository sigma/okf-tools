package bundle

import (
	"sort"
	"testing"
)

// scopedFiles is a bundle with one directory area, one file (glossary) area, and
// out-of-area trees the export contract excludes.
func scopedFiles() map[string]string {
	return map[string]string{
		"okf.toml": "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"areas.json": `{
			"ideas":   {"directory": "ideas",   "type": "idea"},
			"adr":     {"directory": "docs/adr", "type": "adr"},
			"context": {"file": "CONTEXT.md", "type": "context", "role": "glossary"}
		}`,
		"index.md":            "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"ideas/index.md":      "Ideas.\n",
		"ideas/a.md":          concept,
		"docs/adr/0001.md":    concept,
		"CONTEXT.md":          "# Glossary\n",
		"docs/agents/note.md": concept,
		"docs/runbooks/r.md":  concept,
		"loose.md":            concept,
	}
}

func TestInPublishScope(t *testing.T) {
	b := loadBundle(t, scopedFiles())

	// The bare directory rel ("ideas", "docs/adr") is itself in scope as the area
	// root, as is the glossary host file.
	in := []string{"ideas", "ideas/a.md", "ideas/index.md", "docs/adr", "docs/adr/0001.md", "CONTEXT.md"}
	out := []string{"index.md", "docs/agents/note.md", "docs/runbooks/r.md", "loose.md"}

	for _, rel := range in {
		if !b.InPublishScope(rel) {
			t.Errorf("InPublishScope(%q) = false, want true (in a declared area / glossary host)", rel)
		}
	}
	for _, rel := range out {
		if b.InPublishScope(rel) {
			t.Errorf("InPublishScope(%q) = true, want false (outside every declared area)", rel)
		}
	}
}

// A file-backed area that is not the glossary host is not part of the export
// scope: the contract is "the content areas' directories plus the
// glossary/anchor host file", so a stray file area does not publish.
func TestInPublishScopeExcludesNonGlossaryFileArea(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml": "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"areas.json": `{
			"ideas":   {"directory": "ideas", "type": "idea"},
			"context": {"file": "CONTEXT.md", "type": "context", "role": "glossary"},
			"charter": {"file": "CHARTER.md", "type": "charter"}
		}`,
		"index.md":   "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"ideas/a.md": concept,
		"CONTEXT.md": "# Glossary\n",
		"CHARTER.md": concept,
	})
	if b.InPublishScope("CHARTER.md") {
		t.Errorf("a non-glossary file area must not be in publish scope")
	}
	if !b.InPublishScope("CONTEXT.md") {
		t.Errorf("the glossary host file must be in publish scope")
	}
}

func TestPublishDocsNarrowsToScope(t *testing.T) {
	b := loadBundle(t, scopedFiles())

	// The full tree is loaded...
	if len(b.Docs) < 8 {
		t.Fatalf("expected the whole tree loaded, got %d docs", len(b.Docs))
	}

	var got []string
	for _, d := range b.PublishDocs() {
		got = append(got, d.Rel)
	}
	sort.Strings(got)

	want := []string{"CONTEXT.md", "docs/adr/0001.md", "ideas/a.md", "ideas/index.md"}
	if len(got) != len(want) {
		t.Fatalf("PublishDocs rels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PublishDocs rels = %v, want %v", got, want)
		}
	}
}

func TestPublishDocsWithoutAreasReturnsAll(t *testing.T) {
	// An okf.toml-only bundle has no areas registry: nothing to narrow to, so the
	// whole loaded set is the publish set (behaviour is unchanged).
	b := loadBundle(t, map[string]string{
		"okf.toml":       "",
		"index.md":       "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"ideas/a.md":     concept,
		"docs/agents.md": concept,
	})
	if b.Areas != nil {
		t.Fatalf("expected no areas registry")
	}
	if len(b.PublishDocs()) != len(b.Docs) {
		t.Errorf("PublishDocs = %d docs, want all %d (no areas.json to narrow by)", len(b.PublishDocs()), len(b.Docs))
	}
}

// An area's own root README.md is the section-landing page of the area, which
// maps to the unified database itself — not a row in it — so it is out of publish
// scope. A README inside a *sub*directory of the area is a cluster entry point and
// stays in scope (it becomes the nesting parent for its siblings).
func TestInPublishScopeSkipsAreaRootReadme(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml": "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"areas.json": `{
			"specs":   {"directory": "specs", "type": "spec"},
			"context": {"file": "CONTEXT.md", "type": "context", "role": "glossary"}
		}`,
		"index.md":                "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"specs/README.md":         "# Specs\nArea landing page.\n",
		"specs/top.md":            concept,
		"specs/cluster/README.md": "# Cluster\nCluster entry point.\n",
		"specs/cluster/a.md":      concept,
		"CONTEXT.md":              "# Glossary\n",
	})

	if b.InPublishScope("specs/README.md") {
		t.Errorf("area-root README.md must be out of publish scope (not a database row)")
	}
	if !b.InPublishScope("specs/cluster/README.md") {
		t.Errorf("a cluster (subdirectory) README.md must stay in publish scope")
	}
	for _, rel := range []string{"specs/top.md", "specs/cluster/a.md"} {
		if !b.InPublishScope(rel) {
			t.Errorf("InPublishScope(%q) = false, want true (ordinary area page)", rel)
		}
	}

	// PublishDocs reflects the same skip.
	for _, d := range b.PublishDocs() {
		if d.Rel == "specs/README.md" {
			t.Errorf("PublishDocs still includes the area-root README %q", d.Rel)
		}
	}
}
