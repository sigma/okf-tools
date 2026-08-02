// Package notion implements the generic publishing backend against the Notion
// API.
//
// It keeps Notion's block model, its two coupled API limits, and its HTTP
// transport entirely behind the backend role interfaces so that no Notion
// specifics leak into Generation or Optimization.
//
// This package implements the two packing-facing roles — Tokenizer (neutral tree
// → Notion atomic blocks, enforcing the ≤100-blocks/append and per-block char
// caps) and ConstraintModel/Bin (the accumulator that also performs
// create+properties+first-content fusion into one POST /pages). The Executor and
// Scanner (the HTTP client) are a separate ticket (#42).
//
// See sigma/ideas#172 (ratified #163, #164).
package notion

import (
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// Notion's two coupled API limits (the ones this ticket models):
//
//   - maxBlocksPerTxn: a single POST /pages or PATCH /blocks/{id}/children call
//     accepts at most 100 child blocks. The Bin counts a content unit's Cost
//     against this ceiling; a create or properties unit costs zero children (they
//     are the page itself, not its children), which is what makes fusion free.
//   - maxBlockChars: a single Notion rich-text object holds at most 2000
//     characters. The Tokenizer enforces this by splitting an oversized block into
//     several units during tokenization — and because each split adds a block, the
//     char cap feeds directly into the block cap: the two limits are coupled.
const (
	maxBlocksPerTxn = 100
	maxBlockChars   = 2000
)

// Backend implements the Notion Tokenizer and ConstraintModel roles. The Executor
// and Scanner roles (and the shared HTTP client) belong to #42, so a
// notion.Backend does not yet satisfy the backend.Backend umbrella — the
// optimizer depends only on the two roles below.
//
// The zero value is not usable; construct with New. maxBlocks / maxChars are
// configurable so a test can dial packing pressure (a low block cap forces
// overflow; a low char cap forces splitting) exactly as the fake's WithMaxCount
// does — the production defaults are Notion's real limits.
type Backend struct {
	maxBlocks int
	maxChars  int
}

// Compile-time proof that Backend satisfies the two packing-facing roles.
var (
	_ backend.Tokenizer       = (*Backend)(nil)
	_ backend.ConstraintModel = (*Backend)(nil)
)

// Option configures a Backend built by New.
type Option func(*Backend)

// WithMaxBlocksPerTxn overrides the ≤100 child-blocks/transaction ceiling. n <= 0
// keeps the Notion default. A test dials this low to force content overflow across
// several transactions cheaply.
func WithMaxBlocksPerTxn(n int) Option {
	return func(b *Backend) {
		if n > 0 {
			b.maxBlocks = n
		}
	}
}

// WithMaxBlockChars overrides the per-block ≤2000 character cap. n <= 0 keeps the
// Notion default. A test dials this low to force a block to split into several
// units.
func WithMaxBlockChars(n int) Option {
	return func(b *Backend) {
		if n > 0 {
			b.maxChars = n
		}
	}
}

// New builds a Notion backend with Notion's real limits, overridable via options.
func New(opts ...Option) *Backend {
	b := &Backend{maxBlocks: maxBlocksPerTxn, maxChars: maxBlockChars}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// --- The backend payloads (opaque to the pipeline, read only here and by #42) --

// createBlock marks a page-create unit: the shell of a POST /pages. It carries no
// data of its own — the parent id is a Ref the transport resolves at execute
// time, and the properties / first children arrive via fusion in the same Bin.
type createBlock struct {
	node publish.SymbolicID
}

// propsBlock carries a node's neutral properties, to be reshaped into Notion page
// properties when the transaction is serialized (#42). Held neutrally here so no
// Notion property shape leaks earlier than execution.
type propsBlock struct {
	node  publish.SymbolicID
	props map[string]any
}

// deleteBlock marks an archive unit for an orphan node. Like createBlock it holds
// only the node id; archival resolves the real block id from the scan seed.
type deleteBlock struct {
	node publish.SymbolicID
}

// childBlock is one Notion block appended as a child. runs is the ordered inline
// content — literal text spans and late-bound Ref placeholders — kept structured
// so #42 can serialize it into Notion rich text with the Refs resolved. kind and
// level preserve the neutral block's shape (heading level, list depth).
type childBlock struct {
	kind  int // mirrors graph.BlockKind
	level int
	runs  []textRun
}

// textRun is one inline span of a childBlock: literal Text, or a late-bound Ref
// (Text empty, Ref set) the transport resolves to a Notion mention/link.
type textRun struct {
	Text string
	Ref  publish.SymbolicID
}
