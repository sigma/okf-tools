package fix

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
)

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

// TestChangedMatchesApply locks the pending() invariant: Changed reports exactly
// the files Apply writes. On a fixture that needs fixing, Changed is non-empty and
// its length equals Apply's written count; after Apply, a reload reports nothing
// left to change (idempotent).
func TestChangedMatchesApply(t *testing.T) {
	tmp := t.TempDir()
	if err := os.CopyFS(tmp, os.DirFS(filepath.Join("..", "..", "testdata", "okf101"))); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	b := loadFixture(t, tmp)
	set := Enabled(b, nil, nil)

	changed := Changed(b, set)
	if len(changed) == 0 {
		t.Fatal("fixture okf101 should have files needing fixes")
	}

	n, err := Apply(b, set)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n != len(changed) {
		t.Errorf("Apply wrote %d files but Changed reported %d — the pending() invariant is broken", n, len(changed))
	}

	// Reload from the rewritten files: nothing should remain to change.
	b2, err := bundle.Load(b.Root, b.Config.Path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if again := Changed(b2, set); len(again) != 0 {
		t.Errorf("after Apply, Changed still reports %v — fixes are not idempotent", again)
	}
}
