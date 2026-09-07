package gdocs_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/publish/pipeline"
)

// TestMarkerSpansTheRealBody is the #179 regression.
//
// The identity marker's range used to be predicted arithmetically, in the same
// atomic batch as the insert that created the content it covers. The prediction
// was over by one — a rendered body ends in a newline, which merges with the
// paragraph terminator the segment already has, so the segment grows by one unit
// fewer than the string that was sent. The API rejected the range and the whole
// transaction with it.
func TestMarkerSpansTheRealBody(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	be := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), be, loadBundle(t, testBundle())); err != nil {
		t.Fatalf("publish failed on the marker's range: %v", err)
	}

	docID := be.DocumentID()
	// Every published tab carries its marker, so the range was accepted rather
	// than merely omitted.
	for _, title := range fake.tabTitles(docID) {
		if got := fake.namedRangesOf(docID, title); len(got) != 1 {
			t.Errorf("tab %q carries %d marker(s), want exactly 1: %v", title, len(got), got)
		}
	}
}

// TestMarkerRangeSurvivesAContentLengthChange: the off-by-one only shows at the
// END of a body, so a rewrite that changes the length has to stay correct too —
// the case where an arithmetic fix tuned to one fixture would drift.
func TestMarkerRangeSurvivesAContentLengthChange(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	first := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), first, loadBundle(t, testBundle())); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	docID := first.DocumentID()

	for _, replacement := range []string{
		"Alpha links to a much longer sentence than the one it replaced, with more units in it",
		"Alpha x",
	} {
		edited := testBundle()
		edited["alpha.md"] = strings.Replace(edited["alpha.md"], "Alpha links to", replacement, 1)
		be := newBackend(t, srv.URL)
		if _, err := pipeline.Run(context.Background(), be, loadBundle(t, edited)); err != nil {
			t.Fatalf("republish with a %d-character body: %v", len(replacement), err)
		}
		if got := fake.namedRangesOf(docID, "Alpha"); len(got) != 1 {
			t.Errorf("after a rewrite the Alpha tab carries %d marker(s), want 1: %v", len(got), got)
		}
	}
}
