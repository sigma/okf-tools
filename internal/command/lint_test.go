package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/rules"
)

func fixtureDir(name string) string { return filepath.Join("..", "..", "testdata", name) }

func loadFixture(t *testing.T, dir string) *bundle.Bundle {
	t.Helper()
	root, cfg, err := bundle.Discover(dir, "", "")
	if err != nil {
		t.Fatalf("discover %s: %v", dir, err)
	}
	b, err := bundle.Load(root, cfg)
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	return b
}

func runFixture(t *testing.T, dir string) []rules.Finding {
	b := loadFixture(t, dir)
	return rules.Run(&rules.Context{Bundle: b, Config: b.Config}, nil, nil)
}

func ruleCounts(fs []rules.Finding) map[string]int {
	m := map[string]int{}
	for _, f := range fs {
		m[f.Rule]++
	}
	return m
}

func TestHappyBundleClean(t *testing.T) {
	fs := runFixture(t, fixtureDir("happy"))
	if len(fs) != 0 {
		for _, f := range fs {
			t.Logf("unexpected: %s %s:%d %s", f.Rule, f.Path, f.Line, f.Message)
		}
		t.Fatalf("happy bundle should be clean, got %d finding(s)", len(fs))
	}
}

func TestRuleTriggers(t *testing.T) {
	cases := []struct {
		fixture, rule string
	}{
		{"okf001", "OKF001"},
		{"okf002", "OKF002"},
		{"okf003", "OKF003"},
		{"okf004", "OKF004"},
		{"okf101", "OKF101"},
		{"okf102", "OKF102"},
		{"okf103", "OKF103"},
		{"okf104", "OKF104"},
		{"okf105", "OKF105"},
		{"okf106", "OKF106"},
		{"okf107", "OKF107"},
		{"okf201", "OKF201"},
		{"okf202", "OKF202"},
		{"okf206", "OKF206"},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			counts := ruleCounts(runFixture(t, fixtureDir(c.fixture)))
			if counts[c.rule] == 0 {
				t.Errorf("fixture %s: expected rule %s to fire, got %v", c.fixture, c.rule, counts)
			}
		})
	}
}

// TestFootnoteCitations exercises citations.style = "footnote": OKF105 accepts
// well-formed `[^n]:` entries (good.md is clean) and flags a misnumbered one,
// and OKF206 still resolves the inline link inside a footnote definition.
func TestFootnoteCitations(t *testing.T) {
	counts := ruleCounts(runFixture(t, fixtureDir("footnote")))
	if counts["OKF105"] != 1 {
		t.Errorf("footnote: want 1 OKF105 (the misnumbered entry), got %d", counts["OKF105"])
	}
	if counts["OKF206"] != 1 {
		t.Errorf("footnote: want 1 OKF206 (broken citation target), got %d", counts["OKF206"])
	}
}

// TestAutofixResolves copies a fixture to a temp dir, applies its autofixable
// rules, reloads, and asserts the offending rule no longer fires.
func TestAutofixResolves(t *testing.T) {
	cases := []struct{ fixture, rule string }{
		{"okf101", "OKF101"},
		{"okf102", "OKF102"},
		{"okf104", "OKF104"},
		{"okf105", "OKF105"},
		{"okf106", "OKF106"},
		{"footnote", "OKF105"},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			tmp := t.TempDir()
			if err := os.CopyFS(tmp, os.DirFS(fixtureDir(c.fixture))); err != nil {
				t.Fatalf("copy fixture: %v", err)
			}
			b := loadFixture(t, tmp)
			opts := lintFixOptions(b, nil, nil)
			if _, err := applyFixes(b, opts); err != nil {
				t.Fatalf("applyFixes: %v", err)
			}
			b, err := bundle.Load(b.Root, b.Config.Path)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			counts := ruleCounts(rules.Run(&rules.Context{Bundle: b, Config: b.Config}, nil, nil))
			if counts[c.rule] != 0 {
				t.Errorf("fixture %s: rule %s still fires after fix (%d)", c.fixture, c.rule, counts[c.rule])
			}
		})
	}
}
