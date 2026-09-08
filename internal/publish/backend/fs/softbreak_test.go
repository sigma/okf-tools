package fs_test

import (
	"github.com/sigma/okf-tools/internal/bundle/bundletest"
	"path/filepath"
	"strings"
	"testing"
)

// TestSoftWrappedProseSurvivesTheRender is the end-to-end half of #181: the
// shared builder is where the breaks were dropped, but the rendered output is
// where a reader saw "requirementcontradicts". This publishes prose wrapped the
// way the repo's own docs wrap it and reads the file back.
//
// The filesystem backend is the probe because its destination IS text; the defect
// itself was never medium-specific, which is exactly why it reached three
// backends at once.
func TestSoftWrappedProseSurvivesTheRender(t *testing.T) {
	out := t.TempDir()
	b := bundletest.Load(t, map[string]string{
		"okf.toml":   "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md":   "---\nokf_version: \"0.1\"\n---\n# Root\n",
		"CONTEXT.md": "# Glossary\n\n**Root KEK**: the root key-encryption key.\n",
		"docs/b.md":  "---\ntype: adr\ntitle: B\n---\nJust B, standalone.\n",
		"docs/wrapped.md": "---\ntype: adr\ntitle: Wrapped\n---\n" +
			"Five PRDs carrying the requirements of the Notion\n" +
			"*ArboraOS Alpha Blueprint* drafts into this bundle. Every part is\n" +
			"`status: draft`, and no part resolves a disagreement — where a requirement\n" +
			"contradicts something already decided. It cites the [root KEK](../CONTEXT.md#root-kek)\n" +
			"and links [B](b.md)  \nafter a hard break.\n",
	})
	publishToDisk(t, b, out)

	body := readFile(t, filepath.Join(out, "docs", "wrapped.md", "0000.md"))
	for _, want := range []struct{ what, text string }{
		{"a break before an emphasis span", "the Notion ArboraOS Alpha Blueprint drafts"},
		{"a break before a code span", "Every part is status: draft,"},
		{"a plain wrap", "where a requirement contradicts something"},
		// A break whose line ends on a LINK is recorded after it, so the separator
		// has to land outside the linked text rather than inside it.
		{"a break after a resolved link", "root-kek) and links ["},
	} {
		if !strings.Contains(body, want.text) {
			t.Errorf("%s did not survive; body is missing %q:\n%s", want.what, want.text, body)
		}
	}
	// A hard break is a NEWLINE, not a space — the other half of the CommonMark
	// rule, and the one a space-only fix would get wrong.
	if !strings.Contains(body, "\nafter a hard break.") {
		t.Errorf("the hard break did not render as a newline:\n%s", body)
	}
}
