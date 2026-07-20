package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.Links.Style != "any" {
		t.Errorf("Links.Style = %q, want any", c.Links.Style)
	}
	if c.Frontmatter.TimestampFormat != "rfc3339" {
		t.Errorf("TimestampFormat = %q, want rfc3339", c.Frontmatter.TimestampFormat)
	}
	if !c.Index.CheckSync {
		t.Error("Index.CheckSync should default true")
	}
	if c.QMD.Enabled {
		t.Error("QMD.Enabled should default false")
	}
	if c.QMD.NearDuplicateThreshold != 0.85 {
		t.Errorf("NearDuplicateThreshold = %v, want 0.85", c.QMD.NearDuplicateThreshold)
	}
	if c.Gaps.Depth != "direct" || c.Gaps.Top != 10 || c.Gaps.MinSim != 0.4 {
		t.Errorf("Gaps defaults = %+v, want direct/10/0.4", c.Gaps)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "okf.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadOverlay(t *testing.T) {
	p := writeConfig(t, `
[links]
style = "relative"

[qmd]
enabled = true

[rules]
OKF103 = "off"
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Links.Style != "relative" {
		t.Errorf("overridden Links.Style = %q, want relative", c.Links.Style)
	}
	if c.Frontmatter.TimestampFormat != "rfc3339" {
		t.Errorf("absent key should keep default, got %q", c.Frontmatter.TimestampFormat)
	}
	if !c.QMD.Enabled {
		t.Error("QMD.Enabled should be overridden to true")
	}
	if c.Rules["OKF103"] != "off" {
		t.Errorf("Rules[OKF103] = %q, want off", c.Rules["OKF103"])
	}
	if c.Path != p {
		t.Errorf("Path = %q, want %q", c.Path, p)
	}
}

func TestLoadValidateError(t *testing.T) {
	for _, body := range []string{
		"[links]\nstyle = \"sideways\"\n",
		"[frontmatter]\ntimestamp_format = \"iso\"\n",
		"[gaps]\ndepth = \"sideways\"\n",
		"[rules]\nOKF102 = \"loud\"\n",
	} {
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("expected validation error for %q", body)
		}
	}
}

func TestGlossaryDefaults(t *testing.T) {
	c := Default()
	if c.Glossary.Enabled {
		t.Error("Glossary.Enabled should default false")
	}
	if c.Glossary.Files != nil {
		t.Errorf("Glossary.Files should default nil, got %v", c.Glossary.Files)
	}
	// Disabled ⇒ nothing is a glossary, even a name that would match.
	if c.Glossary.Enabled == false && c.IsGlossary("CONTEXT.md") {
		t.Error("IsGlossary should be false when the extension is disabled")
	}
}

func TestGlossaryLoadAndMatch(t *testing.T) {
	p := writeConfig(t, `
[glossary]
enabled = true
files   = ["CONTEXT.md", "src/*/CONTEXT.md"]
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Glossary.Enabled {
		t.Fatal("Glossary.Enabled should be true")
	}
	cases := []struct {
		rel  string
		want bool
	}{
		{"CONTEXT.md", true},          // exact
		{"/CONTEXT.md", true},         // bundle-absolute form
		{"src/auth/CONTEXT.md", true}, // glob
		{"src/CONTEXT.md", false},     // single * does not cross an extra segment
		{"other.md", false},           // no match
		{"CONTEXT.md.bak", false},     // suffix must match exactly
	}
	for _, tc := range cases {
		if got := c.IsGlossary(tc.rel); got != tc.want {
			t.Errorf("IsGlossary(%q) = %v, want %v", tc.rel, got, tc.want)
		}
	}
}

func TestGlossaryBadGlob(t *testing.T) {
	if _, err := Load(writeConfig(t, "[glossary]\nenabled = true\nfiles = [\"[bad\"]\n")); err == nil {
		t.Error("expected validation error for an unparseable glob")
	}
}

func TestReservedSet(t *testing.T) {
	set := Default().ReservedSet()
	if !set["index.md"] || !set["log.md"] {
		t.Errorf("ReservedSet = %v, want index.md and log.md", set)
	}
}
