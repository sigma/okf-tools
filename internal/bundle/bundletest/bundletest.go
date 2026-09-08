// Package bundletest materializes in-memory fixtures as real okf bundles on
// disk and loads them through the production discover/load path, so a test
// drives Stage 1 against genuinely parsed input rather than a hand-built model.
//
// It exists because this was the single most-copied helper in the repo: eleven
// test files across pipeline, transport, graph, backendtest and all three
// backends had each grown their own byte-identical loader, under five different
// names, because there was no seam to stop a twelfth. Fixture semantics — the
// slash-to-native path split, the 0o755/0o644 modes, discovering config rather
// than assuming it — now change in one place.
//
// It cannot be used from internal/bundle's own in-package tests: importing it
// from package bundle is an import cycle. Those keep their local helper.
package bundletest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
)

// Write materializes files (keyed by slash-separated bundle-relative path) into
// a fresh temp directory and returns its root. Use it when a test needs the
// bundle's path on disk; use Load when it needs the parsed bundle.
func Write(t *testing.T, files map[string]string) string {
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

// Load materializes files and loads them through bundle.Discover + bundle.Load,
// failing the test on either error.
func Load(t *testing.T, files map[string]string) *bundle.Bundle {
	t.Helper()
	dir := Write(t, files)
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
