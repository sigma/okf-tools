package graph

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
)

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
func ContentHash(d *bundle.Doc) publish.Hash {
	sum := sha256.Sum256([]byte(d.Content))
	return publish.Hash(hex.EncodeToString(sum[:]))
}
