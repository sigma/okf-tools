package gdocs_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/publish/pipeline"
)

// TestForceRewriteReachesALiveDestination is #183 end to end, on the backend the
// report was found against.
//
// A rendering fix leaves the source untouched, so before the renderer version
// entered the hash a re-run published "0 node(s), 0 anchor(s) in 0
// transaction(s)" and left the destination visibly broken. --force is the escape
// hatch for when that mechanism is missed: it rewrites the live pages rather than
// requiring an operator to reach into the destination and delete its state files
// by hand.
func TestForceRewriteReachesALiveDestination(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	b := loadBundle(t, testBundle())
	first := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), first, b); err != nil {
		t.Fatalf("first run: %v", err)
	}
	docID := first.DocumentID()
	tabsBefore := fake.tabTitles(docID)

	// The steady-state re-run is the reported symptom: nothing to do.
	steady := newBackend(t, srv.URL)
	res, err := pipeline.Run(context.Background(), steady, b)
	if err != nil {
		t.Fatalf("steady re-run: %v", err)
	}
	if res.TxnCount != 0 {
		t.Fatalf("the unchanged re-run emitted %d transaction(s); this fixture is not steady-state", res.TxnCount)
	}

	forced := newBackend(t, srv.URL)
	res, err = pipeline.Run(context.Background(), forced, b, pipeline.WithForceRewrite())
	if err != nil {
		t.Fatalf("forced re-run: %v", err)
	}
	if res.TxnCount == 0 {
		t.Error("--force published nothing: a rendering fix still cannot reach the destination")
	}
	// res.Nodes carries only ids MINTED this run, and a re-assert mints none — the
	// point is that the existing tabs were rewritten, which the body check below is
	// the real evidence for.

	// A rewrite, not a rebuild: the same tabs, still holding their content.
	if got := fake.tabTitles(docID); len(got) != len(tabsBefore) {
		t.Errorf("the forced run left %d tabs, want the original %d: %v", len(got), len(tabsBefore), got)
	}
	if body := fake.tabBody(docID, "Alpha"); !strings.Contains(body, "Alpha links to") {
		t.Errorf("the forced rewrite lost the page's content: %q", body)
	}
}
