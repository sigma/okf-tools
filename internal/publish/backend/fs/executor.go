package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/graph"
)

// Execute serializes one sealed filesystem Transaction into a set of writes under
// the node's export directory, resolving every late-bound Ref through the
// transport's Resolver as it renders, and returns the resolution-table updates the
// writes produced.
//
// The non-fusing policy lives here: rather than collapse the transaction's units
// into a single object (Notion's POST /pages), Execute walks them in order and
// performs one write per unit — the node's directory + node.json for a create,
// props.json for properties, one numbered section file per content unit, and an
// anchors.json when any section hosts an anchor. A delete removes the node's
// exported subtree. The ExecResult reports the created node's on-disk id (its
// export path) and each hosted anchor's id, exactly as the Notion executor reports
// the minted page/block ids — the same seam, a different medium.
func (b *Backend) Execute(_ context.Context, txn publish.Transaction, r backend.Resolver) (publish.ExecResult, error) {
	t, ok := txn.(*Transaction)
	if !ok {
		return publish.ExecResult{}, fmt.Errorf("fs: Execute got a foreign transaction type %T", txn)
	}

	res := publish.ExecResult{
		Nodes:   map[publish.SymbolicID]publish.BackendID{},
		Anchors: map[publish.AnchorName]publish.BackendID{},
	}

	rel := relOfNode(publish.SymbolicID(t.group))
	dir := filepath.Join(b.root, filepath.FromSlash(rel))

	var (
		created   bool
		contentN  int
		anchorMap = map[string]string{} // anchor-name → on-disk id, for anchors.json
	)

	for _, u := range t.units {
		switch p := u.payload.(type) {
		case createBlock:
			// A create re-materializes the node's subtree from scratch (removing any
			// stale export so a re-run is deterministic), then drops the existence
			// marker. RemoveAll of an absent dir is a no-op — the fresh-export case.
			if err := os.RemoveAll(dir); err != nil {
				return res, fmt.Errorf("fs: create %s: %w", rel, err)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return res, fmt.Errorf("fs: create %s: %w", rel, err)
			}
			parent, err := resolveParent(u.refs, r)
			if err != nil {
				return res, fmt.Errorf("fs: create %s: %w", rel, err)
			}
			if err := writeJSON(filepath.Join(dir, "node.json"), nodeMeta{Path: rel, Parent: parent}); err != nil {
				return res, err
			}
			created = true

		case propsBlock:
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return res, fmt.Errorf("fs: properties %s: %w", rel, err)
			}
			if err := writeJSON(filepath.Join(dir, "props.json"), p.props); err != nil {
				return res, err
			}

		case contentBlock:
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return res, fmt.Errorf("fs: content %s: %w", rel, err)
			}
			body, err := renderBlock(p, r)
			if err != nil {
				return res, fmt.Errorf("fs: content %s: %w", rel, err)
			}
			name := fmt.Sprintf("%04d.md", contentN)
			contentN++
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				return res, fmt.Errorf("fs: write %s/%s: %w", rel, name, err)
			}
			for _, a := range p.anchors {
				id := rel + "#" + string(a)
				anchorMap[string(a)] = id
				res.Anchors[a] = publish.BackendID(id)
			}

		case deleteBlock:
			// Archive the orphan by removing its exported subtree. A delete produces
			// nothing and hosts nothing; it stands alone.
			if err := os.RemoveAll(dir); err != nil {
				return res, fmt.Errorf("fs: archive %s: %w", rel, err)
			}
			return res, nil

		default:
			return res, fmt.Errorf("fs: Execute got a foreign payload type %T", u.payload)
		}
	}

	if len(anchorMap) > 0 {
		if err := writeJSON(filepath.Join(dir, "anchors.json"), anchorMap); err != nil {
			return res, err
		}
	}
	// Report the node's on-disk id only when this transaction created it, matching
	// the PackedTxn's Produces (a pure update against a scan-seeded node re-asserts
	// content but mints no new id).
	if created {
		res.Nodes[publish.SymbolicID(t.group)] = publish.BackendID(rel)
	}
	return res, nil
}

// nodeMeta is the node.json existence marker: the node's repo path (so Scan can
// recover its SymbolicID) and its resolved parent on-disk id (empty for a
// top-level node), the filesystem echo of Notion's page-parent.
type nodeMeta struct {
	Path   string `json:"path"`
	Parent string `json:"parent"`
}

// resolveParent resolves a create unit's parent Ref (its single Ref, stamped by
// the optimizer) to a real on-disk id, returning "" for a top-level node whose
// create carries no parent. The transport has already gated the transaction on
// this Ref, so a set-but-unresolved parent is a hard error rather than a silent
// "" — the same loud failure Notion's parentOf makes.
func resolveParent(refs []publish.SymbolicID, r backend.Resolver) (string, error) {
	if len(refs) == 0 {
		return "", nil
	}
	id, ok := r.Resolve(refs[0])
	if !ok {
		return "", fmt.Errorf("parent %s did not resolve", refs[0])
	}
	return string(id), nil
}

// renderBlock serializes one content section to Markdown-ish text, resolving every
// inline Ref through the Resolver — the physical Ref→on-disk-id swap, the exact
// counterpart of the Notion executor turning a Ref into a page mention. An
// unresolved Ref is an error, since the transport must gate on it before Execute.
func renderBlock(cb contentBlock, r backend.Resolver) (string, error) {
	var sb strings.Builder
	switch graph.BlockKind(cb.kind) {
	case graph.Heading:
		level := cb.level
		if level < 1 {
			level = 1
		}
		sb.WriteString(strings.Repeat("#", level))
		sb.WriteByte(' ')
	case graph.ListItem:
		sb.WriteString("- ")
	case graph.Quote:
		sb.WriteString("> ")
	}
	for _, run := range cb.runs {
		if run.Ref != "" {
			id, ok := r.Resolve(run.Ref)
			if !ok {
				return "", fmt.Errorf("content ref %s did not resolve", run.Ref)
			}
			fmt.Fprintf(&sb, "[%s](%s)", refLabel(run.Ref), id)
			continue
		}
		sb.WriteString(run.Text)
	}
	sb.WriteByte('\n')
	return sb.String(), nil
}

// refLabel derives a human-readable link label from a symbolic id by stripping its
// scheme ("node:docs/b.md" → "docs/b.md", "anchor:glossary/root-kek" →
// "glossary/root-kek").
func refLabel(id publish.SymbolicID) string {
	s := string(id)
	if _, rest, ok := strings.Cut(s, ":"); ok {
		return rest
	}
	return s
}

// writeJSON marshals v as indented JSON to path (deterministically — Go sorts map
// keys), creating the file with 0o644.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("fs: marshal %s: %w", path, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("fs: write %s: %w", path, err)
	}
	return nil
}

// relOfNode strips the "node:" scheme from a node symbolic id, yielding the repo
// path that names its export directory. nodeSym is its inverse; the two keep the
// scheme in one place, as Notion's scanner does with its own nodeSym.
func relOfNode(id publish.SymbolicID) string {
	return strings.TrimPrefix(string(id), "node:")
}

// nodeSym mints the node SymbolicID for a repo path — the "node:<path>" scheme
// Generation embeds, so a Ref resolves against a scan seeded through it.
func nodeSym(rel string) publish.SymbolicID {
	return publish.SymbolicID("node:" + rel)
}
