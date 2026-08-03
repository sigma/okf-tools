// Package fs is a filesystem/export backend: a second, real backend that
// implements all four publishing role interfaces against the SAME seam Notion
// does, while stressing the OPPOSITE policy on every axis. It doubles as
// okfpub's dry-run / export mode — a publish that materializes the bundle as a
// tree of files under an output directory instead of touching a live workspace.
//
// It is the standing "the seam didn't leak Notion specifics" proof (sigma/ideas
// #172, decision 7): where Notion has a ≤100-block Bin that fuses create +
// properties + first content into one POST, a per-target rate limit, and an
// HTTP scan, the filesystem backend inverts each axis —
//
//   - Tokenize: one unit per block; each unit becomes its OWN file (a section).
//   - Bin: UNBOUNDED and NON-FUSING — Add never refuses, and Build seals the
//     accumulated units so that Execute performs one write per unit (1 unit = 1
//     write), never collapsing them into a single object.
//   - Group: the node's directory — the export mirrors the node hierarchy on disk.
//   - Rate limit: none (the transport paces per Group, but disk has no 429s).
//   - Scan: a disk read of the exported tree, not a paginated API query.
//
// All four roles absorb both backends without the pipeline (graph → optimize →
// transport) changing, which is the whole point.
package fs

import (
	"context"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// Backend implements all four filesystem backend roles on one struct. The zero
// value writes to and scans the current directory; construct with New to set a
// root. It holds no client and no mutable execution state: every Execute is an
// independent set of filesystem writes under root, so a Backend is safe to reuse
// across runs and across the transport's (currently sequential) drain.
type Backend struct {
	// root is the output directory the export tree is written under and scanned
	// from. Defaults to "." (New sets it); WithRoot points it at, e.g., a
	// t.TempDir() in tests or the CLI's --out directory.
	root string
}

// Compile-time proof that one Backend satisfies all four roles and the umbrella,
// exactly as the Notion and fake backends do — the seam is identical.
var (
	_ backend.Tokenizer       = (*Backend)(nil)
	_ backend.ConstraintModel = (*Backend)(nil)
	_ backend.Executor        = (*Backend)(nil)
	_ backend.Scanner         = (*Backend)(nil)
	_ backend.WriteBacker     = (*Backend)(nil)
	_ backend.Backend         = (*Backend)(nil)
)

// WriteBack is a no-op for the filesystem export. Execute already writes each
// node's full content, ids, and anchors to disk on every run, and Scan re-derives
// them from that tree, so there is no separate provenance to persist between runs
// — the exact inverse of Notion, where write-back into the mirror's derived
// columns is what keeps the steady-state scan cheap. Implemented to satisfy the
// backend.Backend umbrella (and so the transport can call it uniformly); the
// provenance is intentionally dropped.
func (b *Backend) WriteBack(_ context.Context, _ publish.Provenance) error {
	return nil
}

// Option configures a Backend built by New.
type Option func(*Backend)

// WithRoot sets the output directory the export tree is written under and scanned
// from. An empty dir keeps the default (".").
func WithRoot(dir string) Option {
	return func(b *Backend) {
		if dir != "" {
			b.root = dir
		}
	}
}

// New builds a filesystem/export backend rooted at "." by default.
func New(opts ...Option) *Backend {
	b := &Backend{root: "."}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Root reports the output directory the backend writes and scans, so a caller
// (the CLI run summary) can echo where the export landed.
func (b *Backend) Root() string { return b.root }

// --- The backend payloads (opaque to the pipeline, read only here) ----------

// createBlock marks a node-create unit: it materializes the node's directory and
// its node.json existence marker. It carries only the node id; the parent (the
// create unit's one Ref, stamped by the optimizer) is resolved at execute time.
type createBlock struct {
	node publish.SymbolicID
}

// propsBlock carries a node's neutral properties, written verbatim as props.json
// when the transaction is serialized. Held neutrally here so no on-disk shape
// leaks earlier than execution.
type propsBlock struct {
	node  publish.SymbolicID
	props map[string]any
}

// deleteBlock marks an archive unit for an orphan node: Execute removes the
// node's exported subtree. Like createBlock it holds only the node id.
type deleteBlock struct {
	node publish.SymbolicID
}

// contentBlock is one block-level section of a node's content, written as its own
// file (1 unit = 1 write — the non-fusing policy). runs is the ordered inline
// content — literal spans and late-bound Ref placeholders — kept structured so
// Execute can render it with the Refs resolved to on-disk link targets. kind and
// level preserve the neutral block's shape; anchors are the named anchors this
// section hosts, so Execute can map anchor-name → its on-disk id.
type contentBlock struct {
	kind    int // mirrors graph.BlockKind
	level   int
	runs    []textRun
	anchors []publish.AnchorName
	// rows and hasColumnHeader carry a table section's content: rows → cells → the
	// cell's inline runs, header row first. Set only when kind is graph.Table; runs
	// is then empty, since a table's inline content lives per-cell here.
	rows            [][][]textRun
	hasColumnHeader bool
}

// textRun is one inline span of a contentBlock: literal Text, a late-bound Ref
// (Text empty, Ref set) the Executor resolves to an on-disk link target, or a
// literal Text hyperlinked to an external Link URL. Ref and Link are exclusive.
type textRun struct {
	Text string
	Ref  publish.SymbolicID
	// Link, when non-empty, renders Text as a Markdown link to an external URL
	// (the disclaimer banner's source deep-link). Only meaningful when Ref is empty.
	Link string
}
