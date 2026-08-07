// Package notion implements the generic publishing backend against the Notion
// API.
//
// It keeps Notion's block model, its two coupled API limits, and its HTTP
// transport entirely behind the backend role interfaces so that no Notion
// specifics leak into Generation or Optimization.
//
// This package implements all four backend roles on one Backend struct, sharing a
// single Notion HTTP client:
//
//   - Tokenizer (neutral tree → Notion atomic blocks, enforcing the
//     ≤100-blocks/append and per-block char caps) — tokenize.go;
//   - ConstraintModel/Bin (the accumulator that also performs
//     create+properties+first-content fusion into one POST /pages) — bin.go;
//   - Executor (serialize an opaque Transaction into POST /pages, PATCH children,
//     or an archive, substituting late-bound Refs via the transport's Resolver as
//     it writes, and returning the ExecResult table updates) — executor.go;
//   - Scanner in both provenance modes — the cheap steady-state ScanStored (one
//     paginated data-source query over self-describing derived columns) and the
//     opt-in ScanRecompute (a full live block walk that recomputes hashes and
//     self-heals subpage/anchor ids) — scanner.go and recompute.go;
//   - WriteBacker (the publish-time obligation that persists ids/hashes/anchors
//     back into the derived columns and subtree map so the next ScanStored is
//     accurate) — writeback.go.
//
// See sigma/ideas#172 (ratified #163, #164, #167).
package notion

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/schema"
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

// Backend implements all four Notion backend roles on one struct, so a single
// notion.Backend satisfies the backend.Backend umbrella and shares one HTTP client
// across its Executor and Scanner.
//
// The zero value is not usable; construct with New. maxBlocks / maxChars are
// configurable so a test can dial packing pressure (a low block cap forces
// overflow; a low char cap forces splitting) exactly as the fake's WithMaxCount
// does — the production defaults are Notion's real limits. The HTTP fields are the
// shared client the Executor and Scanner both trade through: baseURL and http are
// injectable so tests drive an httptest server offline, with no live workspace.
// That shared client also owns the rate-limit defenses — global pacing and retry
// — because it is the one point every Notion request passes through (#129).
type Backend struct {
	maxBlocks int
	maxChars  int

	// baseURL is the Notion API root (no trailing slash); token and notionVersion
	// are the auth / API-version headers; dataSourceID is the NOTION_DB_ID the
	// scan queries and top-level page-creates parent under; http is the shared
	// request doer (an *http.Client, or any doer a test injects).
	baseURL       string
	token         string
	notionVersion string
	dataSourceID  string
	http          httpDoer

	// limits is the rate-limit policy every request inherits from do — the global
	// pacing gate and the retry schedule (see client.go). logf is where the backend
	// reports operationally, currently the retry notices, so a throttled run is
	// visible in its output rather than merely slow.
	limits limiter
	logf   func(format string, args ...any)

	// schema is the parsed schema.json driving two things: provisioning (the
	// Provisioner role reconciles the data source's columns against it) and typed
	// property serialization (propsJSON keys each value's Notion shape off the
	// column's declared Kind). Nil when no schema.json was configured — then
	// provisioning is a no-op and property values fall back to the legacy
	// title/rich_text split.
	schema *schema.Schema
}

// Compile-time proof that one Backend satisfies all four roles and the umbrella.
var (
	_ backend.Tokenizer       = (*Backend)(nil)
	_ backend.ConstraintModel = (*Backend)(nil)
	_ backend.Executor        = (*Backend)(nil)
	_ backend.Scanner         = (*Backend)(nil)
	_ backend.WriteBacker     = (*Backend)(nil)
	_ backend.Backend         = (*Backend)(nil)
	_ backend.Provisioner     = (*Backend)(nil)
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

// WithBaseURL points the shared HTTP client at a different API root — an httptest
// server in tests, so the Executor and Scanner run offline. A trailing slash is
// trimmed. Empty keeps the default.
func WithBaseURL(u string) Option {
	return func(b *Backend) {
		if u != "" {
			b.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// WithToken sets the Notion integration token sent as the Bearer credential.
func WithToken(t string) Option {
	return func(b *Backend) { b.token = t }
}

// WithDataSourceID sets the NOTION_DB_ID the scan queries and top-level page
// creates parent under.
func WithDataSourceID(id string) Option {
	return func(b *Backend) { b.dataSourceID = id }
}

// WithHTTPClient injects the HTTP client the Executor and Scanner share. A nil
// client keeps the default. A test injects an *http.Client whose Transport is a
// recorded round-tripper, or simply the default client aimed at an httptest server
// via WithBaseURL.
func WithHTTPClient(c *http.Client) Option {
	return func(b *Backend) {
		if c != nil {
			b.http = c
		}
	}
}

// WithNotionVersion overrides the Notion-Version header. Empty keeps the default.
func WithNotionVersion(v string) Option {
	return func(b *Backend) {
		if v != "" {
			b.notionVersion = v
		}
	}
}

// WithInterval sets the global minimum spacing between two Notion requests — the
// pacing every call inherits from the shared request chokepoint, whatever page or
// route it targets. A non-positive interval disables pacing entirely, which is the
// setting the offline tests run under so they take no wall-clock delay.
func WithInterval(d time.Duration) Option {
	return func(b *Backend) { b.limits.interval = d }
}

// WithLogger redirects the backend's operational reporting — currently the retry
// notices, which must reach the run's output so a chronically throttled publish is
// visible rather than merely slow. The default writes to stderr. A nil logger is
// ignored; pass a no-op function to silence it.
func WithLogger(logf func(format string, args ...any)) Option {
	return func(b *Backend) {
		if logf != nil {
			b.logf = logf
		}
	}
}

// withClock overrides the clock seam (now + sleep). Unexported: it exists only so
// the package's own tests can drive pacing and retry backoff off a virtual clock.
func withClock(now func() time.Time, sleep func(context.Context, time.Duration) error) Option {
	return func(b *Backend) {
		b.limits.now = now
		b.limits.sleep = sleep
	}
}

// WithSchema hands the backend the parsed schema.json. It drives both the
// Provisioner role (which columns to reconcile onto the data source, and with
// which Notion types) and typed property serialization (each value's shape keyed
// off its column's Kind). A nil schema is accepted and leaves provisioning a
// no-op and property values on their legacy title/rich_text split.
func WithSchema(s *schema.Schema) Option {
	return func(b *Backend) { b.schema = s }
}

// New builds a Notion backend with Notion's real limits, a default HTTP client
// aimed at the public API, and the rate-limit defenses on (global pacing at
// DefaultInterval, bounded retry of 429/5xx, retries reported to stderr) — all
// overridable via options.
func New(opts ...Option) *Backend {
	b := &Backend{
		maxBlocks:     maxBlocksPerTxn,
		maxChars:      maxBlockChars,
		baseURL:       defaultBaseURL,
		notionVersion: defaultNotionVersion,
		http:          http.DefaultClient,
		limits: limiter{
			interval:    DefaultInterval,
			maxAttempts: defaultMaxAttempts,
			now:         time.Now,
			sleep:       realSleep,
		},
		logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// --- The backend payloads (opaque to the pipeline, read only here and by #42) --

// createBlock marks a page-create unit: the shell of a POST /pages. The properties
// and first children arrive via fusion in the same Bin. parent is the node's parent
// symbolic id ("" for a top-level node); the transport resolves it to the real
// parent page id at execute time, and its emptiness is also what tells the create
// which KIND of object it is making (a data-source row or a child_page).
type createBlock struct {
	node   publish.SymbolicID
	parent publish.SymbolicID
}

// propsBlock carries a node's neutral properties, to be reshaped into Notion page
// properties when the transaction is serialized (#42). Held neutrally here so no
// Notion property shape leaks earlier than execution.
//
// parent is the node's parent symbolic id ("" for a top-level node). It is carried
// on the properties unit — not just the create unit — because the parent KIND
// selects which properties may be written at all: a page-parented node is a
// child_page with no data-source columns. Without it a standalone property update
// against a subpage PATCHes columns that do not exist and 400s (#128).
type propsBlock struct {
	node   publish.SymbolicID
	parent publish.SymbolicID
	props  map[string]any
}

// deleteBlock marks an archive unit for an orphan node. Like createBlock it holds
// only the node id; archival resolves the real block id from the scan seed.
type deleteBlock struct {
	node publish.SymbolicID
}

// childBlock is one Notion block appended as a child. runs is the ordered inline
// content — literal text spans and late-bound Ref placeholders — kept structured
// so the Executor can serialize it into Notion rich text with the Refs resolved.
// kind and level preserve the neutral block's shape (heading level, list depth).
// anchors are the named anchors this block hosts: the Bin threads them onto the
// block from its AtomicUnit so the Executor, once the block has a real Notion id,
// can report anchor-name → block-id in its ExecResult (the anchor map).
type childBlock struct {
	kind     int // mirrors graph.BlockKind
	level    int
	language string // fence language for a code block; empty otherwise
	runs     []publish.Run
	anchors  []publish.AnchorName
	// assertsContent marks the first block of a node's content assertion — the
	// tokenizer sets it on the first unit it emits for a Document, and only there.
	// The Bin promotes it to the sealed Transaction's AssertsContent flag, which is what
	// distinguishes "assert this node's whole content" from "append this
	// continuation chunk" (sigma/okf-tools#130). It is metadata, not payload: it
	// never reaches the wire.
	assertsContent bool
	// rows and hasColumnHeader carry a table block's content: rows → cells → the
	// cell's inline runs, header row first. Set only when kind is graph.Table; runs
	// is then empty, since a table's inline content lives per-cell here.
	rows            []tableRow
	hasColumnHeader bool
}

// tableRow is one row of a table childBlock: its cells left-to-right, each cell an
// ordered run of inline spans (literal text, Ref placeholders, or hyperlinks) the
// Executor serializes into a Notion table_row cell with Refs resolved.
type tableRow struct {
	cells [][]publish.Run
}
