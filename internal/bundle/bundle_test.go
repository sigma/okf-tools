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
