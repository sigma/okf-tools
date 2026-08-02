package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/sigma/okf-tools/internal/publish"
)

// Scan produces the neutral CurrentState by READING THE EXPORTED TREE FROM DISK —
// the deliberate inverse of Notion's paginated HTTP query. It walks root, treats
// every directory holding a node.json existence marker as a mirrored node, and
// reconstructs:
//
//   - NodeID: node:<path> → the node's on-disk id (its export path), recovered
//     from node.json;
//   - AnchorID: anchor-name → on-disk id, recovered from the sibling anchors.json.
//
// It does not reconstruct ContentHash: the filesystem export re-materializes each
// node's content on every run (a dry-run/export makes a full snapshot, not a
// steady-state diff), so change-detection re-asserts existing nodes rather than
// hash-skipping them. NodeID is still recovered, so a re-export against a populated
// tree correctly emits SetProperties+SetContent (an update) rather than a second
// CreateNode.
//
// A missing root is an empty snapshot (the fresh-export case), not an error.
func (b *Backend) Scan(_ context.Context) (*publish.CurrentState, error) {
	nodeIDs := map[publish.SymbolicID]publish.BackendID{}
	anchorIDs := map[publish.AnchorName]publish.BackendID{}

	if _, err := os.Stat(b.root); err != nil {
		if os.IsNotExist(err) {
			return publish.NewCurrentState(nil, nil, nil), nil
		}
		return nil, fmt.Errorf("fs: scan: stat %s: %w", b.root, err)
	}

	walkErr := filepath.WalkDir(b.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "node.json" {
			return nil
		}
		dir := filepath.Dir(path)

		meta, err := readNodeMeta(path)
		if err != nil {
			return err
		}
		rel := meta.Path
		if rel == "" {
			// Recover the path from the directory position if the marker omitted it.
			r, relErr := filepath.Rel(b.root, dir)
			if relErr != nil {
				return relErr
			}
			rel = filepath.ToSlash(r)
		}
		nodeIDs[nodeSym(rel)] = publish.BackendID(rel)

		if err := readAnchors(filepath.Join(dir, "anchors.json"), anchorIDs); err != nil {
			return err
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("fs: scan: %w", walkErr)
	}

	return publish.NewCurrentState(nodeIDs, nil, anchorIDs), nil
}

// readNodeMeta reads a node.json existence marker.
func readNodeMeta(path string) (nodeMeta, error) {
	var m nodeMeta
	data, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("fs: scan: read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("fs: scan: parse %s: %w", path, err)
	}
	return m, nil
}

// readAnchors folds a node's anchors.json (anchor-name → on-disk id) into the
// anchor table so a page linking into an already-exported glossary resolves its
// anchors straight from the scan seed. A missing file contributes nothing.
func readAnchors(path string, anchorIDs map[publish.AnchorName]publish.BackendID) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("fs: scan: read %s: %w", path, err)
	}
	var am map[string]string
	if err := json.Unmarshal(data, &am); err != nil {
		return fmt.Errorf("fs: scan: parse %s: %w", path, err)
	}
	for name, id := range am {
		anchorIDs[publish.AnchorName(name)] = publish.BackendID(id)
	}
	return nil
}
