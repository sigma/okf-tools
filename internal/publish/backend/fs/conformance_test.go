package fs_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/publish/backend/backendtest"
	fsbackend "github.com/sigma/okf-tools/internal/publish/backend/fs"
)

// TestConformance runs the cross-backend kit against the filesystem backend,
// whose destination is a temp tree rather than a fake API.
func TestConformance(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backendtest.Subject {
		out := t.TempDir()
		return backendtest.Subject{
			Backend: fsbackend.New(fsbackend.WithRoot(out)),
			// This backend rewrites its tree rather than counting requests, so it
			// answers the determinism half of the near-noop pair: an unchanged
			// re-export must produce a byte-identical tree.
			Snapshot: func() string { return treeSnapshot(t, out) },
		}
	})
}

// treeSnapshot renders the exported tree as one comparable string: every file's
// path and content, in a stable order.
func treeSnapshot(t *testing.T, root string) string {
	t.Helper()
	var sb strings.Builder
	var paths []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(paths)
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		rel, _ := filepath.Rel(root, p)
		sb.WriteString("--- " + filepath.ToSlash(rel) + "\n")
		sb.Write(raw)
	}
	return sb.String()
}
