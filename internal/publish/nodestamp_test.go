package publish

import "testing"

// FillMissing is the first-non-empty fold the optimizer's accumulator applies as
// it gathers a bin's units: an already-set field wins, an empty one is filled from
// the incoming stamp. Every field folds independently, so a node whose stamp
// arrives split across units (hashes on the content unit, title on the create
// unit) still assembles a complete stamp.
func TestNodeStampFillMissing(t *testing.T) {
	// A set field is never overwritten; an empty one is filled.
	base := NodeStamp{Hash: "h1", Title: "keep"}
	base.FillMissing(NodeStamp{Hash: "h2", PropHash: "p2", Parent: "node:x", Title: "lose"})
	want := NodeStamp{Hash: "h1", PropHash: "p2", Parent: "node:x", Title: "keep"}
	if base != want {
		t.Errorf("FillMissing = %+v, want %+v", base, want)
	}

	// Filling from an all-empty stamp is a no-op.
	full := NodeStamp{Hash: "h", PropHash: "p", Parent: "node:y", Title: "t"}
	got := full
	got.FillMissing(NodeStamp{})
	if got != full {
		t.Errorf("FillMissing(zero) = %+v, want unchanged %+v", got, full)
	}
}
