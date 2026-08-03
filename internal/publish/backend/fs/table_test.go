package fs_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// A GFM table in a source doc publishes end-to-end (parser → graph → tokenize →
// fs Execute) as a rendered GFM pipe table, header row, delimiter row, and body
// rows — the filesystem echo of #106's Notion fix. Before it, the table fell through
// to a Generic block and rendered as a flat paragraph of pipe text.
func TestExportRendersTable(t *testing.T) {
	files := map[string]string{
		"okf.toml": "",
		"index.md": "---\nokf_version: \"0.1\"\n---\n# Root\n",
		"t.md":     "---\ntype: adr\ntitle: T\n---\n| Name | Role |\n| --- | --- |\n| Ada | author |\n",
	}
	b := loadBundle(t, files)
	out := t.TempDir()
	publishToDisk(t, b, out)

	body := readFile(t, filepath.Join(out, "t.md", "0000.md"))
	wantLines := []string{
		"| Name | Role |",
		"| --- | --- |",
		"| Ada | author |",
	}
	for _, ln := range wantLines {
		if !strings.Contains(body, ln) {
			t.Errorf("rendered table missing line %q; got:\n%s", ln, body)
		}
	}
}

// A cross-document link inside a table cell resolves to its on-disk target, so a
// reference authored in a table still forms and renders its edge — the same
// resolution the flat blocks get, threaded through a cell.
func TestExportTableCellLinkResolves(t *testing.T) {
	files := map[string]string{
		"okf.toml": "",
		"index.md": "---\nokf_version: \"0.1\"\n---\n# Root\n",
		"b.md":     "---\ntype: adr\ntitle: B\n---\nJust B.\n",
		"t.md":     "---\ntype: adr\ntitle: T\n---\n| See |\n| --- |\n| [B](b.md) |\n",
	}
	b := loadBundle(t, files)
	out := t.TempDir()
	publishToDisk(t, b, out)

	body := readFile(t, filepath.Join(out, "t.md", "0000.md"))
	if !strings.Contains(body, "(b.md)") {
		t.Errorf("table cell link did not resolve to docs/b.md on disk:\n%s", body)
	}
}
