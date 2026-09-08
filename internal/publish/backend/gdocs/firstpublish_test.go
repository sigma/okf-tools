package gdocs_test

import (
	"context"
	"github.com/sigma/okf-tools/internal/bundle/bundletest"
	"net/http"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/publish/backend/gdocs"
	"github.com/sigma/okf-tools/internal/publish/pipeline"
)

// TestFirstPublishIntoAFreshDestination is the #176 regression, and it is
// specifically the case the suite never had: every existing test that reached
// writeTab did so against a tab whose identity marker already existed, because
// the same run had just written it.
//
// A first publish has no marker to delete. Asking the API to delete one is a
// hard 400 rather than a no-op, and batchUpdate is atomic, so the whole
// transaction failed and no content was ever written.
func TestFirstPublishIntoAFreshDestination(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	be := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), be, bundletest.Load(t, testBundle())); err != nil {
		t.Fatalf("the FIRST publish into an empty destination failed: %v", err)
	}

	docID := be.DocumentID()
	if got := fake.tabTitles(docID); len(got) == 0 {
		t.Fatal("no tabs were published")
	}
	// Content really landed: an atomic batch that 400s writes nothing at all, so
	// asserting the run returned no error is not enough.
	if body := fake.tabBody(docID, "Alpha"); !strings.Contains(body, "Alpha links to") {
		t.Errorf("the first publish wrote no content to the Alpha tab: %q", body)
	}
}

// TestRewriteStillReassertsTheMarker guards the other side of the guard. The
// marker is re-created on every rewrite BECAUSE the API documents only how a
// named range is adjusted as content shifts, never what happens when the span it
// covers is deleted outright (#158) — so skipping the delete when the marker IS
// present would leave the undocumented case live, or duplicate the range.
func TestRewriteStillReassertsTheMarker(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	b := bundletest.Load(t, testBundle())
	first := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), first, b); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	docID := first.DocumentID()

	// A second run against the same destination, with content that CHANGED, so the
	// tab is genuinely rewritten rather than hash-skipped.
	edited := testBundle()
	edited["alpha.md"] = strings.Replace(edited["alpha.md"], "Alpha links to", "Alpha now links to", 1)
	second := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), second, bundletest.Load(t, edited)); err != nil {
		t.Fatalf("republish: %v", err)
	}

	if body := fake.tabBody(docID, "Alpha"); !strings.Contains(body, "Alpha now links to") {
		t.Fatalf("the rewrite did not land: %q", body)
	}
	// Exactly one marker survives: the rewrite deleted the old one and re-asserted
	// it, rather than stacking a second range under the same name.
	want := "okf:alpha.md"
	var found int
	for _, name := range fake.namedRangesOf(docID, "Alpha") {
		if name == want {
			found++
		}
	}
	if found != 1 {
		t.Errorf("the Alpha tab carries %d %q marker(s), want exactly 1 (all: %v)",
			found, want, fake.namedRangesOf(docID, "Alpha"))
	}
}

// TestHalfProvisionedDestinationIsResumable is the other half of "first
// publish": the one that already half-happened. Provisioning succeeds before the
// content write, so the run that #176 broke still left its document and sidecar
// behind, and the re-run after the fix must adopt them rather than create a
// second set beside them — a claim the failure path now leans on and nothing
// tested (#176).
func TestHalfProvisionedDestinationIsResumable(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	// Stand in for the failed run: provisioning succeeded, no content was written.
	aborted := newBackend(t, srv.URL)
	if err := aborted.Provision(context.Background()); err != nil {
		t.Fatalf("provision: %v", err)
	}
	provisioned := aborted.DocumentID()
	filesAfterProvision := len(fake.fileIDs())

	// The re-run finds the destination by its identity key, not by title.
	resumed := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), resumed, bundletest.Load(t, testBundle())); err != nil {
		t.Fatalf("the re-run after a half-provisioned destination failed: %v", err)
	}
	if resumed.DocumentID() != provisioned {
		t.Errorf("the re-run published into %s, want the already-provisioned %s",
			resumed.DocumentID(), provisioned)
	}
	if got := len(fake.fileIDs()); got != filesAfterProvision {
		t.Errorf("the re-run created %d extra Drive file(s); a half-provisioned "+
			"destination must be adopted, not duplicated", got-filesAfterProvision)
	}
	// Adoption is not enough on its own: the re-run has to actually finish the
	// work the failed one never started.
	if body := fake.tabBody(resumed.DocumentID(), "Alpha"); !strings.Contains(body, "Alpha links to") {
		t.Errorf("the resumed run published no content: %q", body)
	}
	// The sidecar is adopted too — it is provisioned alongside the document, so a
	// second one would be the same duplication in the state file.
	if sc := fake.sidecar(); !strings.Contains(sc, "alpha.md") {
		t.Errorf("the resumed run wrote no provenance to the adopted sidecar: %q", sc)
	}
}

// TestDryRunAgainstAnExistingDestinationReads covers the read this fix added.
//
// A dry run against a destination that EXISTS now reads it, because reading
// mutates nothing and the dump should describe the requests a real run would
// issue against that destination — including whether a marker is there to
// delete. The guarantee that a dry run touches nothing is unaffected, and is
// asserted here rather than assumed.
func TestDryRunAgainstAnExistingDestinationReads(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	b := bundletest.Load(t, testBundle())
	live := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), live, b); err != nil {
		t.Fatalf("seed publish: %v", err)
	}
	filesBefore, writesBefore := len(fake.fileIDs()), fake.batchUpdateCount()
	bodyBefore := fake.tabBody(live.DocumentID(), "Alpha")

	var dump strings.Builder
	dry, err := gdocs.New(context.Background(), gdocs.Config{
		DriveID: testDriveID, Bundle: "testbundle", Selection: "concepts",
		DocsEndpoint: srv.URL, DriveEndpoint: srv.URL, HTTPClient: &http.Client{},
		DryRunWriter: &dump,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	edited := testBundle()
	edited["alpha.md"] = strings.Replace(edited["alpha.md"], "Alpha links to", "Alpha now links to", 1)
	if _, err := pipeline.Run(context.Background(), dry, bundletest.Load(t, edited)); err != nil {
		t.Fatalf("dry run against an existing destination: %v", err)
	}

	// Touched nothing: no new file, no write, no changed content.
	if got := len(fake.fileIDs()); got != filesBefore {
		t.Errorf("the dry run created %d Drive file(s)", got-filesBefore)
	}
	if got := fake.batchUpdateCount(); got != writesBefore {
		t.Errorf("the dry run issued %d batchUpdate(s)", got-writesBefore)
	}
	if got := fake.tabBody(live.DocumentID(), "Alpha"); got != bodyBefore {
		t.Errorf("the dry run changed live content:\n before %q\n after  %q", bodyBefore, got)
	}
	// And it planned the rewrite it would really perform — against a tab whose
	// marker exists, so the delete belongs in the dump this time.
	out := dump.String()
	if !strings.Contains(out, "deleteNamedRange") {
		t.Errorf("the dump omits the marker delete a real rewrite would issue:\n%s", out)
	}
}
