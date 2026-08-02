package publish

import (
	"os/exec"
	"strings"
	"testing"

	// Blank-import the whole publisher subtree so this test package's build graph
	// includes it. That ties the symmetric check's `go test` result cache to the
	// subtree: if any subpackage gains a forbidden import, this package's action
	// ID changes and the test re-runs even on a warm cache.
	//
	// The lean-lint check below shells out to `go list` on cmd/okftool, which is
	// NOT part of this package's build graph, so a warm GOCACHE can report a
	// stale pass for it. Enforcement therefore relies on a *fresh* run: the
	// `nix flake check` CI job runs the suite in a sandbox with no host GOCACHE,
	// and the plain `go` CI job passes `-count=1` (see .github/workflows/ci.yml).
	// Locally, run `go test -count=1 ./...` to bypass the cache.
	_ "github.com/sigma/okf-tools/internal/publish/backend"
	_ "github.com/sigma/okf-tools/internal/publish/backend/notion"
	_ "github.com/sigma/okf-tools/internal/publish/graph"
	_ "github.com/sigma/okf-tools/internal/publish/optimize"
	_ "github.com/sigma/okf-tools/internal/publish/transport"
)

const modulePath = "github.com/sigma/okf-tools"

// deps returns the transitive import set (including the package itself) of the
// given package pattern, via `go list -deps`. It uses a fully-qualified pattern
// so the result is independent of this test's working directory, and parses
// stdout only so toolchain diagnostics on stderr can't pollute the dep set.
func deps(t *testing.T, pattern string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", pattern).Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("go list -deps %s: %v\n%s", pattern, err, stderr)
	}
	var pkgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			pkgs = append(pkgs, line)
		}
	}
	return pkgs
}

// importsForbidden reports the first forbidden path that dep is, or lives under.
// Matching the path itself or any subpackage (path + "/") keeps both boundary
// directions equally strict.
func importsForbidden(dep string, forbidden ...string) string {
	for _, f := range forbidden {
		if dep == f || strings.HasPrefix(dep, f+"/") {
			return f
		}
	}
	return ""
}

// TestLintBinaryStaysLean enforces the lean-lint-binary invariant: the lint
// binary okftool must never pull the publisher-only internal/publish subtree
// (and its Notion client / HTTP transport) into its dependency closure.
func TestLintBinaryStaysLean(t *testing.T) {
	for _, dep := range deps(t, modulePath+"/cmd/okftool") {
		if hit := importsForbidden(dep, modulePath+"/internal/publish"); hit != "" {
			t.Errorf("cmd/okftool must not depend on the publisher subtree, but imports %s", dep)
		}
	}
}

// TestPublishDoesNotImportLintOnly is the symmetric half of the invariant: the
// publisher subtree internal/publish/** must never import the lint-only
// internal/command or internal/rules packages (or any subpackage of them), so
// the seam holds in both directions.
func TestPublishDoesNotImportLintOnly(t *testing.T) {
	for _, dep := range deps(t, modulePath+"/internal/publish/...") {
		if hit := importsForbidden(dep, modulePath+"/internal/command", modulePath+"/internal/rules"); hit != "" {
			t.Errorf("internal/publish/** must not import lint-only package %s (under %s)", dep, hit)
		}
	}
}
