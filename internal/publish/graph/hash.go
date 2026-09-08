package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"maps"
	"slices"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
)

// RendererVersion is folded into every default content hash, and is BUMPED
// whenever this package changes what a given source renders into.
//
// Without it the hash covered only the inputs, so a fix to the RENDERER could
// never reach an already-published page: two okfpub versions that render the same
// source differently produced the same hash, change detection skipped every node,
// and the run reported "0 transaction(s)" and success while the mirror kept
// serving the old output. That happened twice running, with #181 (soft line
// breaks dropped) and #182 (nested tables flattened) — both verified-correct
// fixes that a plain re-run could not deliver (#183).
//
// Bumping it re-renders every page exactly once. The cost is REMEMBERING to bump
// it, which is worth naming plainly: forgetting reproduces the bug silently.
// Bump it in the same commit as any change to how buildDocument projects source
// into blocks — new block kinds, changed inline handling, changed text folding —
// and leave it alone for anything a reader of the mirror cannot see. The escape
// hatch when it is missed is --force (WithForceRewrite), which is why the two
// ship together.
//
// It starts at 2, not 1: every hash written before this existed is version 1 by
// construction, and introducing the prefix is itself the bump — the one that
// delivers #181 (soft/hard line breaks now render) and #182 (blocks nested in a
// list item or quote survive as blocks) to destinations already published.
const RendererVersion = 2

// ContentHash is the default expected-state hash of a source doc: a SHA-256 over
// its whole source (frontmatter + body). Change detection compares it against the
// scanned hash a node carries — equal means hash-skip, different means the
// "changed / drifted" arm (SetProperties + SetContent).
//
// It covers the frontmatter, not just the body, because a node's mirrored state
// is both its properties and its content: a title/type edit with an untouched
// body must still re-assert, so the single expected hash gates the whole changed
// arm the way the contract's diff→ops table intends.
//
// The shared subtree-hash reconstruction a real backend does over its live state
// (#146/#167) is the scan producer's concern; a backend whose scanner rebuilds a
// matching hash swaps this out via WithHasher so both sides agree. Both sides
// sharing one algorithm is the whole contract; the pipeline treats the value as
// opaque.
func ContentHash(d *bundle.Doc) publish.Hash { return contentHashAt(RendererVersion, d) }

// contentHashAt is ContentHash at an explicit renderer version. Separated so a
// test can prove a bump moves every hash without a global to mutate.
func contentHashAt(version int, d *bundle.Doc) publish.Hash {
	dig := NewContentDigestAt(version)
	dig.Writef("%s", d.Content)
	return dig.Sum()
}

// ContentDigest is a content hash under construction that has ALREADY folded in
// the renderer version. It is the seam that makes RendererVersion an obligation a
// hasher cannot forget rather than one it must remember.
//
// There is more than one content hasher — the default one over source bytes, and
// the Notion backend's over the realized block stream, which REPLACES it — and
// every one of them must fold the version in or a renderer fix cannot reach an
// already-published page (#183). That used to be enforced by a comment naming its
// own failure mode: "the cost is REMEMBERING to bump it… forgetting reproduces the
// bug silently". The bug is silent because the run reports success while the mirror
// keeps serving the old output.
//
// Starting from a constructor instead means a new hasher gets the version by
// construction. The unseeded alternative is not reachable: this type has no useful
// zero value, so the only way to a content hash is through NewContentDigest.
type ContentDigest struct{ h hash.Hash }

// NewContentDigest opens a content digest seeded with the current renderer
// version. Every content hasher starts here.
func NewContentDigest() *ContentDigest { return NewContentDigestAt(RendererVersion) }

// NewContentDigestAt opens a content digest at an explicit renderer version, so a
// test can prove a bump moves every hash without a global to mutate. Production
// code calls NewContentDigest.
//
// The version is a PREFIX with a NUL separator rather than a suffix or a plain
// concatenation, so no hashed content can forge another version's hash.
func NewContentDigestAt(version int) *ContentDigest {
	h := sha256.New()
	fmt.Fprintf(h, "okf/render/%d\x00", version)
	return &ContentDigest{h: h}
}

// Write folds raw bytes into the digest. It never returns an error (hash.Hash
// does not), so callers need not check one.
func (d *ContentDigest) Write(p []byte) (int, error) { return d.h.Write(p) }

// Writef folds formatted content into the digest — the shape both hashers use,
// one writing a document's source, the other a line per canonical block.
func (d *ContentDigest) Writef(format string, a ...any) { fmt.Fprintf(d.h, format, a...) }

// Sum seals the digest into the hash the pipeline compares.
func (d *ContentDigest) Sum() publish.Hash {
	return publish.Hash(hex.EncodeToString(d.h.Sum(nil)))
}

// PropertyHash is the expected-state hash of a doc's semantic properties — the
// SetProperties arm's change signal, split out from ContentHash so a title/type/
// frontmatter edit re-asserts properties without a body rewrite and a body edit
// never forces a property write (#110 phase 2). It fingerprints exactly what
// SetProperties asserts, propsOf(d) (frontmatter + derived title + type), as a
// SHA-256 over the properties in sorted-key order with each value JSON-encoded, so
// it is stable across runs and independent of map iteration order.
//
// Unlike the content hash, it needs no round-trip alignment with a live scan: the
// recompute path reads it back from stored state rather than reconstructing it from
// live Notion properties, so only cross-run stability matters here.
func PropertyHash(d *bundle.Doc) publish.Hash {
	props := propsOf(d)
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(props)) {
		// Frontmatter values are YAML-decoded scalars/lists/maps, so json.Marshal
		// encodes them deterministically (nested map keys sorted too). Fall back to a
		// Go-syntax rendering for any value it cannot encode, so two distinct such
		// values never collide into the same (empty) encoding and hide a drift.
		v, err := json.Marshal(props[k])
		if err != nil {
			v = []byte(fmt.Sprintf("%#v", props[k]))
		}
		fmt.Fprintf(h, "%s=%s\n", k, v)
	}
	return publish.Hash(hex.EncodeToString(h.Sum(nil)))
}
