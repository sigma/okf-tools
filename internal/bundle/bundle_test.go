package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBundle(t *testing.T, files map[string]string) string {
	t.Helper()
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
	return dir
}

func loadBundle(t *testing.T, files map[string]string) *Bundle {
	t.Helper()
	dir := writeBundle(t, files)
	root, cfgPath, err := Discover(dir, "", "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	b, err := Load(root, cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return b
}

// A README.md symlink to index.md is kept for GitHub folder rendering while
// index.md stays the OKF-native canonical index. okfpub must publish the real
// index.md once and treat the symlink as invisible — otherwise the walk emits
// two pages for one document. See #91.
func TestSymlinkedFileSkipped(t *testing.T) {
	dir := writeBundle(t, map[string]string{
		"okf.toml": "# x\n",
		"index.md": "---\nokf_version: \"0.1\"\n---\n# root\n",
		"a.md":     concept,
	})
	if err := os.Symlink("index.md", filepath.Join(dir, "README.md")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	root, cfgPath, err := Discover(dir, "", "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	b, err := Load(root, cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if d := b.byRel["README.md"]; d != nil {
		t.Errorf("README.md symlink was published as %+v; want it skipped", d)
	}
	if b.byRel["index.md"] == nil {
		t.Error("index.md (the symlink target) should still be published")
	}
	// The target document must be published exactly once: index.md + a.md, with
	// the README.md symlink contributing no extra row.
	var rels []string
	for _, d := range b.Docs {
		rels = append(rels, d.Rel)
	}
	if len(b.Docs) != 2 {
		t.Errorf("published %d docs %v; want exactly 2 (index.md, a.md)", len(b.Docs), rels)
	}
}

const concept = "---\ntype: Concept\ndescription: x\n---\nBody.\n"

func TestDiscover(t *testing.T) {
	t.Run("okf.toml", func(t *testing.T) {
		dir := writeBundle(t, map[string]string{"okf.toml": "# x\n", "a.md": concept})
		_, cfg, err := Discover(dir, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(cfg, "okf.toml") {
			t.Errorf("config path = %q, want .../okf.toml", cfg)
		}
	})
	t.Run("index okf_version", func(t *testing.T) {
		dir := writeBundle(t, map[string]string{
			"index.md": "---\nokf_version: \"0.1\"\n---\n# c\n* [a](a.md)\n",
			"a.md":     concept,
		})
		_, cfg, err := Discover(dir, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if cfg != "" {
			t.Errorf("config path = %q, want empty (no okf.toml)", cfg)
		}
	})
	t.Run("not found", func(t *testing.T) {
		if _, _, err := Discover(t.TempDir(), "", ""); err == nil {
			t.Error("expected discovery error for a bare directory")
		}
	})
}

func TestClassify(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml": "[links]\nstyle = \"relative\"\n",
		"a.md": "---\ntype: T\ndescription: x\n---\n" +
			"Cross [b](b.md), abs [c](/c.md), url [x](https://e.com), img ![i](i.png), anchor [s](#sec), wiki [[b]].\n\n" +
			"# Citations\n\n[1] [src](../import/s.md)\n",
		"b.md": concept,
		"c.md": concept,
	})
	a := b.byRel["a.md"]
	if a == nil {
		t.Fatal("a.md not loaded")
	}
	byTarget := map[string]ResolvedLink{}
	for _, rl := range a.Resolved {
		byTarget[rl.Target] = rl
	}
	cases := []struct {
		target string
		class  Class
	}{
		{"b.md", ClassConcept},
		{"/c.md", ClassConcept},
		{"https://e.com", ClassExternal},
		{"i.png", ClassImage},
		{"#sec", ClassAnchor},
		{"b", ClassWikilink},
		{"../import/s.md", ClassCitation},
	}
	for _, c := range cases {
		rl, ok := byTarget[c.target]
		if !ok {
			t.Errorf("link %q not found", c.target)
			continue
		}
		if rl.Class != c.class {
			t.Errorf("link %q class = %d, want %d", c.target, rl.Class, c.class)
		}
	}
	if rl := byTarget["b.md"]; !rl.Inside || !rl.Exists || rl.TargetDoc == nil {
		t.Errorf("b.md link should resolve inside+exist+target, got %+v", rl)
	}
	if rl := byTarget["/c.md"]; !rl.Absolute {
		t.Error("/c.md link should be Absolute")
	}
}

func TestOwnerScope(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml":     "# x\n",
		"index.md":     "---\nokf_version: \"0.1\"\n---\n# c\n* [a](a.md)\n",
		"a.md":         concept,
		"sub/index.md": "# c\n* [x](x.md)\n",
		"sub/x.md":     concept,
	})
	root := b.byRel["index.md"]
	subIdx := b.byRel["sub/index.md"]
	a := b.byRel["a.md"]
	x := b.byRel["sub/x.md"]

	if b.Owner(a) != root {
		t.Error("a.md should be owned by the root index")
	}
	if b.Owner(x) != subIdx {
		t.Error("sub/x.md should be owned by the sub index")
	}
	scope := b.Scope(root)
	if len(scope) != 1 || scope[0] != a {
		t.Errorf("root scope = %v, want [a.md] only (sub/x.md is owned by sub index)", scope)
	}
}

func TestRenderIndex(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml": "# x\n",
		"index.md": "---\nokf_version: \"0.1\"\n---\n# c\n",
		"a.md":     "---\ntype: Concept\ntitle: A\ndescription: The A.\n---\nBody.\n",
	})
	out := b.RenderIndex(b.byRel["index.md"])
	if !strings.Contains(out, "* [A](a.md) - The A.") {
		t.Errorf("RenderIndex missing entry, got:\n%s", out)
	}
	if !strings.Contains(out, "okf_version") {
		t.Error("root index should keep okf_version frontmatter")
	}
}

func TestRelSlash(t *testing.T) {
	cases := []struct{ from, target, want string }{
		{".", "a.md", "a.md"},
		{"sub", "a.md", "../a.md"},
		{"sub", "sub/x.md", "x.md"},
		{"a/b", "a/c.md", "../c.md"},
	}
	for _, c := range cases {
		if got := RelSlash(c.from, c.target); got != c.want {
			t.Errorf("RelSlash(%q,%q) = %q, want %q", c.from, c.target, got, c.want)
		}
	}
}

func TestResolveWikilink(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml": "# x\n",
		"b.md":     concept,
		"sub/b.md": concept, // makes basename "b" ambiguous
		"uniq.md":  concept,
	})
	if b.ResolveWikilink("uniq") == nil {
		t.Error("unambiguous 'uniq' should resolve")
	}
	if b.ResolveWikilink("b") != nil {
		t.Error("ambiguous 'b' (two files) should not resolve")
	}
	if b.ResolveWikilink("nope") != nil {
		t.Error("unknown target should not resolve")
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Root KEK":            "root-kek",
		"Foreign-rooted leaf": "foreign-rooted-leaf",
		"Re-share":            "re-share",
		"re share":            "re-share",
		"  Trailing  ":        "trailing",
		// GitHub's github-slugger maps each whitespace char to a hyphen without
		// collapsing runs: punctuation stripped from between two spaces leaves a
		// double hyphen. See OKF203 (okf-tools#116).
		"C++ & Go!": "c--go",
		"TL;DR — the landscape splits into three layers":       "tldr--the-landscape-splits-into-three-layers",
		"Standing obligation — a consumer of the resource set": "standing-obligation--a-consumer-of-the-resource-set",
		// Leading/trailing hyphens are NOT trimmed (only surrounding
		// whitespace is), matching github-slugger and the resolve.ts reference:
		// a stripped em-dash at an edge leaves an edge hyphen.
		"— Edge": "-edge",
		"Edge —": "edge-",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFragmentPreserved(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"index.md":   "---\nokf_version: \"0.1\"\n---\n- [a](a.md)\n",
		"a.md":       "---\ntype: Concept\ndescription: x\n---\nSee [root](/CONTEXT.md#root-kek) and [self](#local).\n",
		"CONTEXT.md": "**Root KEK**: def\n",
	})
	a := b.byRel["a.md"]
	if a == nil {
		t.Fatal("a.md not loaded")
	}
	var concept, anchor *ResolvedLink
	for i := range a.Resolved {
		switch a.Resolved[i].Class {
		case ClassConcept:
			concept = &a.Resolved[i]
		case ClassAnchor:
			anchor = &a.Resolved[i]
		}
	}
	if concept == nil || concept.Fragment != "root-kek" {
		t.Errorf("concept link Fragment = %+v, want root-kek", concept)
	}
	if anchor == nil || anchor.Fragment != "local" {
		t.Errorf("anchor link Fragment = %+v, want local", anchor)
	}
}

func TestGlossaryAnchors(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml":   "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md":   "- [c](CONTEXT.md)\n",
		"CONTEXT.md": "# Keys\n\n**Root KEK**: the top of the hierarchy.\n\n- **Foreign-rooted leaf**: a leaf.\n",
	})
	g := b.byRel["CONTEXT.md"]
	if g == nil || !g.Glossary {
		t.Fatalf("CONTEXT.md should be a glossary doc, got %+v", g)
	}
	for _, want := range []string{"root-kek", "foreign-rooted-leaf", "keys"} {
		if !g.HasAnchor(want) {
			t.Errorf("glossary is missing anchor %q (have %+v)", want, g.Anchors)
		}
	}
	if g.HasAnchor("nope") {
		t.Error("HasAnchor(nope) should be false")
	}
	// A non-glossary file has no anchors.
	if idx := b.byRel["index.md"]; idx != nil && idx.Glossary {
		t.Error("index.md should not be a glossary")
	}
}

// docByRel finds a loaded doc by its bundle-relative path, failing the test if
// it is absent.
func docByRel(t *testing.T, b *Bundle, rel string) *Doc {
	t.Helper()
	for _, d := range b.Docs {
		if d.Rel == rel {
			return d
		}
	}
	t.Fatalf("no doc with rel %q", rel)
	return nil
}

// A frontmatter-less page must title itself by its top-level H1 and type itself
// from the area that owns it (areas.json) — matching the production mirror,
// rather than emitting the rel path as title and an empty type. See #99.
func TestTitleAndTypeFallbacks(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml": "# x\n",
		"areas.json": `{
		  "research": { "directory": "docs/research", "type": "research" },
		  "ideas":    { "directory": "ideas", "type": "idea" }
		}`,
		// no frontmatter, opens with an H1, under a declared area
		"docs/research/notion.md": "# Notion API: schema mutation\n\nbody\n",
		// no frontmatter, no H1
		"ideas/no-h1.md": "just prose, no heading\n",
		// authored frontmatter wins over both fallbacks
		"ideas/authored.md": "---\ntitle: Authored Title\ntype: essay\n---\n\n# Ignored H1\n",
		// frontmatter-less, not under any area -> empty type
		"loose/orphan.md": "# Loose Note\n\nbody\n",
	})

	if d := docByRel(t, b, "docs/research/notion.md"); d.Title() != "Notion API: schema mutation" || d.Type() != "research" {
		t.Errorf("research page: Title=%q Type=%q, want (\"Notion API: schema mutation\", \"research\")", d.Title(), d.Type())
	}
	if d := docByRel(t, b, "ideas/no-h1.md"); d.Title() != "ideas/no-h1.md" {
		t.Errorf("no-H1 page: Title=%q, want the rel path", d.Title())
	}
	if d := docByRel(t, b, "ideas/no-h1.md"); d.Type() != "idea" {
		t.Errorf("no-H1 page: Type=%q, want \"idea\" (area fallback)", d.Type())
	}
	if d := docByRel(t, b, "ideas/authored.md"); d.Title() != "Authored Title" || d.Type() != "essay" {
		t.Errorf("authored page: Title=%q Type=%q, want (\"Authored Title\", \"essay\")", d.Title(), d.Type())
	}
	if d := docByRel(t, b, "loose/orphan.md"); d.Title() != "Loose Note" || d.Type() != "" {
		t.Errorf("orphan page: Title=%q Type=%q, want (\"Loose Note\", \"\")", d.Title(), d.Type())
	}
}

// With no areas.json at all, a frontmatter-less page still degrades to an empty
// type (and H1 title) — the registry is optional and its absence is not an error.
func TestTypeFallbackNoAreasRegistry(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml": "# x\n",
		"a.md":     "# A Heading\n\nbody\n",
	})
	d := docByRel(t, b, "a.md")
	if d.Title() != "A Heading" {
		t.Errorf("Title=%q, want \"A Heading\"", d.Title())
	}
	if d.Type() != "" {
		t.Errorf("Type=%q, want \"\" (no registry)", d.Type())
	}
}

// An explicitly empty frontmatter `type: ""` is treated as unauthored, so the
// area fallback applies — symmetric with Title()'s empty-string guard. This
// pins the "missing key or empty value" intent behind Type()'s t != "" check.
func TestEmptyFrontmatterTypeFallsBackToArea(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml":            "# x\n",
		"areas.json":          `{ "ideas": { "directory": "ideas", "type": "idea" } }`,
		"ideas/blank-type.md": "---\ntype: \"\"\n---\n\n# Blank Type\n",
	})
	if d := docByRel(t, b, "ideas/blank-type.md"); d.Type() != "idea" {
		t.Errorf("empty-string type: Type=%q, want \"idea\" (area fallback)", d.Type())
	}
}
