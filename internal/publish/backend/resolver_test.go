package backend_test

import (
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// stubResolver is a resolution table with no overlay, standing in for the
// transport's own.
type stubResolver map[publish.SymbolicID]publish.BackendID

func (s stubResolver) Resolve(id publish.SymbolicID) (publish.BackendID, bool) {
	b, ok := s[id]
	return b, ok
}

// block is a stand-in for the per-backend payload each adapter collects anchors
// from — childBlock in Notion, contentBlock in Docs, unit on the filesystem.
type block struct{ anchors []publish.AnchorName }

func anchorsOf(b block) []publish.AnchorName { return b.anchors }

// TestHostedAnchorsCollectsTheUnion covers what every backend needs before it can
// discharge the optimizer's self-anchor suppression: the set of anchors this
// transaction hosts, unioned across its write targets.
func TestHostedAnchorsCollectsTheUnion(t *testing.T) {
	hosted := backend.HostedAnchors([]block{
		{anchors: []publish.AnchorName{"glossary/root-kek"}},
		{},
		{anchors: []publish.AnchorName{"glossary/dek", "glossary/root-kek"}},
	}, anchorsOf)

	if len(hosted) != 2 {
		t.Fatalf("hosted = %v, want the two distinct anchor names", hosted)
	}
	for _, want := range []publish.AnchorName{"glossary/root-kek", "glossary/dek"} {
		if !hosted[want] {
			t.Errorf("hosted is missing %q", want)
		}
	}
}

// TestHostedAnchorsIsNilWhenNoneHosted keeps the common case free: a transaction
// hosting no anchors must not allocate, so callers can nil-check rather than
// iterate.
func TestHostedAnchorsIsNilWhenNoneHosted(t *testing.T) {
	if got := backend.HostedAnchors([]block{{}, {}}, anchorsOf); got != nil {
		t.Errorf("HostedAnchors over anchorless blocks = %v, want nil", got)
	}
}

// TestMintOverlayResolvesHostedAnchorsLocally is the point of the seam: an anchor
// the transaction hosts itself resolves DURING Execute, even though the transport
// table cannot answer it — the optimizer suppressed that Ref precisely because this
// transaction satisfies it.
func TestMintOverlayResolvesHostedAnchorsLocally(t *testing.T) {
	base := stubResolver{"node:a.md": "page-a"}
	hosted := map[publish.AnchorName]bool{"glossary/root-kek": true}

	r := backend.MintOverlay(base, hosted, func(a publish.AnchorName) publish.BackendID {
		return publish.BackendID("CONTEXT.md#" + string(a))
	})

	got, ok := r.Resolve(publish.AnchorRef("glossary/root-kek"))
	if !ok {
		t.Fatal("a self-hosted anchor must resolve through the overlay; the transport table cannot answer it")
	}
	if want := publish.BackendID("CONTEXT.md#glossary/root-kek"); got != want {
		t.Errorf("minted id = %q, want %q", got, want)
	}
}

// TestMintOverlayFallsThroughToBase proves the overlay is scoped to self-hosted
// anchors only: a cross-document link still resolves through the transport table,
// which is what gates the transaction in the first place.
func TestMintOverlayFallsThroughToBase(t *testing.T) {
	base := stubResolver{"node:a.md": "page-a"}
	r := backend.MintOverlay(base, map[publish.AnchorName]bool{"glossary/dek": true},
		func(publish.AnchorName) publish.BackendID { return "placeholder" })

	if got, ok := r.Resolve("node:a.md"); !ok || got != "page-a" {
		t.Errorf("base resolution = %q,%v, want page-a,true", got, ok)
	}
	if _, ok := r.Resolve("node:missing.md"); ok {
		t.Error("an id neither hosted nor in the table must stay unresolved")
	}
}

// TestMintOverlayWithNoAnchorsReturnsBase keeps the no-anchor case free of a
// wrapper, and mint must never be called when there is nothing to mint.
func TestMintOverlayWithNoAnchorsReturnsBase(t *testing.T) {
	base := stubResolver{"node:a.md": "page-a"}
	r := backend.MintOverlay(base, nil, func(publish.AnchorName) publish.BackendID {
		t.Fatal("mint must not be called when the transaction hosts no anchors")
		return ""
	})
	if _, ok := r.(stubResolver); !ok {
		t.Errorf("MintOverlay with no hosted anchors should return the base unwrapped, got %T", r)
	}
}

// TestResolveRunsErrorsOnUnresolvedRef pins the contract the whole seam rests on:
// a Ref that reaches Execute unresolved is a wiring violation, not a data
// condition, so it fails loudly rather than emitting a dangling citation.
func TestResolveRunsErrorsOnUnresolvedRef(t *testing.T) {
	runs := []publish.Run{{Text: "see "}, {Text: "B", Ref: "node:b.md"}}
	if _, err := backend.ResolveRuns(runs, stubResolver{}); err == nil {
		t.Fatal("an unresolved content ref must be an error")
	}

	got, err := backend.ResolveRuns(runs, stubResolver{"node:b.md": "page-b"})
	if err != nil {
		t.Fatalf("ResolveRuns: %v", err)
	}
	if got[0].RefID != "" {
		t.Errorf("a literal run must carry no resolved id, got %q", got[0].RefID)
	}
	if got[1].RefID != "page-b" {
		t.Errorf("reference run resolved to %q, want page-b", got[1].RefID)
	}
}
