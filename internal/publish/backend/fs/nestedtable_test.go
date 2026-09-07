package fs_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNestedTableKeepsItsSeparators is the #182 regression at the render end: a
// table indented inside a list item reached the builder as INLINE content, so it
// was published as running text with no cell or row separators at all — and with
// the item's own sentence glued to the first cell.
func TestNestedTableKeepsItsSeparators(t *testing.T) {
	out := t.TempDir()
	b := loadBundle(t, map[string]string{
		"okf.toml":   "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md":   "---\nokf_version: \"0.1\"\n---\n# Root\n",
		"CONTEXT.md": "# Glossary\n\n**Root KEK**: the root key-encryption key.\n",
		"docs/nested.md": "---\ntype: adr\ntitle: Nested\n---\n" +
			"8. **Retention must be bounded.** The two models are not reconcilable\n" +
			"   by adjusting a number:\n\n" +
			"   | Field | Draft 2 | Here |\n" +
			"   |---|---|---|\n" +
			"   | Raw messages | 90 days | Run-scoped |\n",
	})
	publishToDisk(t, b, out)

	body := sectionsOf(t, filepath.Join(out, "docs", "nested.md"))
	for _, want := range []string{
		"Field | Draft 2 | Here",
		"Raw messages | 90 days | Run-scoped",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the nested table lost its separators; body is missing %q:\n%s", want, body)
		}
	}
	// The block boundary is lost too when the table is flattened: the report shows
	// "number:" glued straight onto the first cell.
	if strings.Contains(body, "number:Field") || strings.Contains(body, "number: Field") {
		t.Errorf("the list item's own text merged into the table's first cell:\n%s", body)
	}
}

// sectionsOf concatenates a node's rendered sections in order: the backend splits
// a page across NNNN.md files, and which section a block lands in is not what
// these tests are about.
func sectionsOf(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var sb strings.Builder
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		sb.WriteString(readFile(t, filepath.Join(dir, e.Name())))
		sb.WriteString("\n")
	}
	return sb.String()
}
