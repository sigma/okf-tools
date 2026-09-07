package gdocs_test

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/publish/pipeline"
)

// TestMarkedTabsAreAdoptedWhenStateIsBehind is the #180 regression: the failure
// a crash leaves behind.
//
// Provisioning and the tab writes succeed, the sidecar write does not — so the
// document holds correctly marked tabs the state file has never heard of. The
// state file was the only thing the scan seeded tabs from, so every later run
// planned a CREATE for a tab that already existed, and the API rejected the
// duplicate title. The destination was then unpublishable forever, recoverable
// only by deleting it by hand.
func TestMarkedTabsAreAdoptedWhenStateIsBehind(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	first := newBackend(t, srv.URL)
	b := loadBundle(t, testBundle())
	if _, err := pipeline.Run(context.Background(), first, b); err != nil {
		t.Fatalf("first run: %v", err)
	}
	docID := first.DocumentID()
	before := fake.tabTitles(docID)
	alphaTab := fake.tabIDOf(docID, "Alpha")

	// Stand in for the crash: the tabs are on the document, the provenance for all
	// but one never made it to the sidecar.
	keepOnly(t, fake, "CONTEXT.md")

	second := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), second, loadBundle(t, testBundle())); err != nil {
		t.Fatalf("the re-run after a lost sidecar failed; a marked tab must be adopted, not re-created: %v", err)
	}

	after := fake.tabTitles(docID)
	if len(after) != len(before) {
		t.Errorf("the re-run left %d tabs, want %d: %v", len(after), len(before), after)
	}
	// Adoption means the SAME tab, not a fresh one wearing the same title: a new
	// tab would strand every cross-tab link that targets the old one.
	if got := fake.tabIDOf(docID, "Alpha"); got != alphaTab {
		t.Errorf("the Alpha tab is now %s, want the adopted %s", got, alphaTab)
	}
	if body := fake.tabBody(docID, "Alpha"); !strings.Contains(body, "Alpha links to") {
		t.Errorf("the adopted tab lost its content: %q", body)
	}
	// The sidecar is a cache of a scan that can always be rebuilt, so the run that
	// rebuilt it must also write it back.
	if sc := fake.sidecar(); !strings.Contains(sc, alphaTab) {
		t.Errorf("the re-run did not re-record the adopted tab %s: %s", alphaTab, sc)
	}
}

// TestPreExistingTitlesAreDisambiguated is the second half of #180: the
// claimed-title map was only ever filled by createTab, so on a run against an
// existing document it started empty and disambiguate could not see a single tab
// already there. A new page whose fitted title collides with an existing tab's
// then produced the same duplicate-title rejection, for a different reason.
func TestPreExistingTitlesAreDisambiguated(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	base := map[string]string{
		"okf.toml": "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md": "---\nokf_version: \"0.1\"\ntitle: Index\ntype: index\n---\n\n# Index\n",
		"CONTEXT.md": "---\nokf_version: \"0.1\"\ntitle: Context\ntype: context\n---\n\n" +
			"# Keys\n\n- **Widget**: a small thing.\n",
		"alpha/page.md": "---\nokf_version: \"0.1\"\ntitle: \"" + longestTitle + "\"\ntype: concept\n---\n\n# One\n",
	}
	first := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), first, loadBundle(t, base)); err != nil {
		t.Fatalf("first run: %v", err)
	}
	docID := first.DocumentID()

	// The second page arrives in a LATER run, so its collision is with a tab that
	// already exists rather than with one this run created.
	grown := maps.Clone(base)
	grown["beta/page.md"] = "---\nokf_version: \"0.1\"\ntitle: \"" + longestTitle + "\"\ntype: concept\n---\n\n# Two\n"

	second := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), second, loadBundle(t, grown)); err != nil {
		t.Fatalf("a new page colliding with an existing tab's title failed to publish: %v", err)
	}

	titles := fake.tabTitles(docID)
	seen := map[string]int{}
	for _, title := range titles {
		seen[title]++
	}
	for title, n := range seen {
		if n > 1 {
			t.Errorf("%d tabs share the title %q", n, title)
		}
	}
	t.Logf("titles: %v", titles)
}

// testSidecar is the state file as these tests edit it: the node payloads stay
// opaque, so a test standing in for drifted state does not have to restate the
// backend's own state shape.
type testSidecar struct {
	Version int            `json:"version"`
	Nodes   map[string]any `json:"nodes"`
}

func parseSidecar(t *testing.T, fake *fakeGoogle) testSidecar {
	t.Helper()
	var sc testSidecar
	if err := json.Unmarshal([]byte(fake.sidecar()), &sc); err != nil {
		t.Fatalf("parse sidecar: %v", err)
	}
	return sc
}

func writeSidecar(t *testing.T, fake *fakeGoogle, sc testSidecar) {
	t.Helper()
	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	fake.setSidecar(string(raw))
}

// keepOnly rewrites the sidecar down to the named nodes, standing in for a run
// that wrote its tabs and died before persisting the rest of its provenance.
func keepOnly(t *testing.T, fake *fakeGoogle, rels ...string) {
	t.Helper()
	sc := parseSidecar(t, fake)
	kept := map[string]any{}
	for _, rel := range rels {
		ns, ok := sc.Nodes[rel]
		if !ok {
			t.Fatalf("sidecar has no node %q; fixture is stale: %v", rel, sc.Nodes)
		}
		kept[rel] = ns
	}
	sc.Nodes = kept
	writeSidecar(t, fake, sc)
}

// TestASidecarPointingAtAnotherNodesTabIsIgnored: the sidecar can disagree with
// the document in BOTH directions, and only one of them is the crash case.
//
// Here the sidecar names a tab whose marker belongs to a different node — what a
// hand-edited or partially-rewritten sidecar produces. Trusting it would put two
// nodes on one tab, and since every write replaces a tab's whole body, the second
// to write would erase the first. The marker on the tab is the tie-break (#180).
func TestASidecarPointingAtAnotherNodesTabIsIgnored(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	first := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), first, loadBundle(t, testBundle())); err != nil {
		t.Fatalf("first run: %v", err)
	}
	docID := first.DocumentID()
	betaTab := fake.tabIDOf(docID, "Beta")
	repoint(t, fake, "alpha.md", betaTab)

	second := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), second, loadBundle(t, testBundle())); err != nil {
		t.Fatalf("re-run with a cross-claiming sidecar failed: %v", err)
	}

	if got := fake.tabIDOf(docID, "Alpha"); got == betaTab {
		t.Fatalf("alpha.md was adopted onto Beta's tab %s; the marker on that tab names beta.md", betaTab)
	}
	// Each page still has its own content, which is what the clobber would have cost.
	if body := fake.tabBody(docID, "Alpha"); !strings.Contains(body, "Alpha links to") {
		t.Errorf("the Alpha tab holds %q", body)
	}
	if body := fake.tabBody(docID, "Beta"); !strings.Contains(body, "Beta stands alone") {
		t.Errorf("the Beta tab holds %q", body)
	}
}

// TestAnAdoptedOrphanIsReclaimed is what adoption makes newly REACHABLE, and it
// deletes rather than writes, so it is worth pinning on its own.
//
// A tab whose source page is gone used to be invisible to the scan once the
// sidecar lost its entry, so it leaked: it stayed in the document forever, with
// no way to name it. Adopting it from its marker puts it back in the scan, where
// the ordinary vanished-node path reclaims it (#180).
func TestAnAdoptedOrphanIsReclaimed(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	files := testBundle()
	files["gamma.md"] = "---\nokf_version: \"0.1\"\ntitle: Gamma\ntype: concept\n---\n\n# Gamma\n\nGamma stands alone.\n"

	first := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), first, loadBundle(t, files)); err != nil {
		t.Fatalf("first run: %v", err)
	}
	docID := first.DocumentID()
	if fake.tabIDOf(docID, "Gamma") == "" {
		t.Fatal("the first run published no Gamma tab")
	}

	// The page goes away, and the sidecar entry that named its tab goes with it.
	delete(files, "gamma.md")
	keepOnly(t, fake, "CONTEXT.md")

	second := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), second, loadBundle(t, files)); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if fake.tabIDOf(docID, "Gamma") != "" {
		t.Errorf("the tab of a deleted page survived: %v", fake.tabTitles(docID))
	}
	if fake.tabIDOf(docID, "Alpha") == "" {
		t.Errorf("reclaiming the orphan took a live tab with it: %v", fake.tabTitles(docID))
	}
}

// repoint rewrites one node's recorded tab in the sidecar, standing in for state
// that has drifted from the document.
func repoint(t *testing.T, fake *fakeGoogle, rel, tab string) {
	t.Helper()
	sc := parseSidecar(t, fake)
	ns, ok := sc.Nodes[rel].(map[string]any)
	if !ok {
		t.Fatalf("sidecar has no node %q: %v", rel, sc.Nodes)
	}
	ns["tab"] = tab
	sc.Nodes[rel] = ns
	writeSidecar(t, fake, sc)
}
