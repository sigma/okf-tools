package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

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
