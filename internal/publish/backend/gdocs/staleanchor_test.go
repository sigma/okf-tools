package gdocs_test

import (
	"context"
	"fmt"
	"github.com/sigma/okf-tools/internal/bundle/bundletest"
	"net/http"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/publish/backend/gdocs"
	"github.com/sigma/okf-tools/internal/publish/pipeline"
)

// lateGlossaryBundle is the shape that strands a link (#171). Two things have to
// coincide, and both are ordinary:
//
//   - the glossary occupies MORE THAN ONE transaction, which a few hundred terms
//     achieves by passing the per-transaction request ceiling; each of those
//     transactions rewrites the whole tab and re-mints its heading ids;
//   - the citing page's transaction is ordered BEFORE the glossary's later one,
//     which comes down to filename sort — hence "zz-", where a CONTEXT.md would
//     sort ahead of its citing pages and mask the bug entirely.
func lateGlossaryBundle(terms int) map[string]string {
	ctx := "---\nokf_version: \"0.1\"\ntitle: Glossary\ntype: context\n---\n\n# Keys\n\n"
	for i := 0; i < terms; i++ {
		ctx += fmt.Sprintf("- **Term %d**: definition number %d with some words in it.\n", i, i)
	}
	return map[string]string{
		"okf.toml":       "[glossary]\nenabled = true\nfiles = [\"zz-glossary.md\"]\n",
		"zz-glossary.md": ctx,
		"alpha.md": "---\nokf_version: \"0.1\"\ntitle: Alpha\ntype: concept\n---\n\n" +
			"# Alpha\n\nAlpha cites [Term 0](/zz-glossary.md#term-0).\n",
	}
}

// assertNoStaleLinks is the property #171 is about: at the end of a run, every
// heading link in the document points at a heading id the target tab still
// carries. A stale one renders as an ordinary working link and goes nowhere.
func assertNoStaleLinks(t *testing.T, fake *fakeGoogle, docID string) {
	t.Helper()
	for _, title := range fake.tabTitles(docID) {
		for _, l := range fake.linksOf(docID, title) {
			h, ok := l["heading"].(map[string]any)
			if !ok {
				continue
			}
			id, _ := h["id"].(string)
			target, _ := h["tabId"].(string)
			if !fake.headingIDsOfTab(docID, target)[id] {
				t.Errorf("tab %q links to heading %q in tab %s, which no longer carries it",
					title, id, target)
			}
		}
	}
}

// TestRewriteDoesNotStrandCrossTabLinks is the #171 regression.
func TestRewriteDoesNotStrandCrossTabLinks(t *testing.T) {
	cases := []struct {
		terms      int
		wantWrites int // how many times the hosting tab must be rewritten
	}{
		{terms: 300, wantWrites: 2},
		{terms: 500, wantWrites: 3},
	}
	for _, tc := range cases {
		terms, wantWrites := tc.terms, tc.wantWrites
		t.Run(fmt.Sprint(terms), func(t *testing.T) {
			fake := newFakeGoogle(t)
			srv := fake.server()
			defer srv.Close()

			be := newBackend(t, srv.URL)
			if _, err := pipeline.Run(context.Background(), be, bundletest.Load(t, lateGlossaryBundle(terms))); err != nil {
				t.Fatalf("run: %v", err)
			}
			t.Logf("writes: %v", fake.writeLog)
			docID := be.DocumentID()
			assertNoStaleLinks(t, fake, docID)

			// The hosting tab really was rewritten, which is the precondition the whole
			// bug needs; without pinning it, a change to the per-transaction request
			// ceiling would quietly turn this into a test of nothing.
			writes := countWrites(fake.writeLog, "insert:Glossary")
			if writes < wantWrites {
				t.Errorf("the glossary tab was written %d time(s), need at least %d for this case",
					writes, wantWrites)
			}

			// A correct target over the wrong span is still a broken citation, so
			// assert the repaired link covers the citation's own text.
			linked := fake.linkedTexts(docID, "Alpha")
			link, ok := linked["term-0"]
			if !ok {
				t.Fatalf("no link covers the citation text %q; covered spans: %v", "term-0", keysOf(linked))
			}
			h, _ := link["heading"].(map[string]any)
			if h == nil {
				t.Fatalf("the citation is not a heading link: %v", link)
			}
			if id, _ := h["id"].(string); !fake.headingIDsOfTab(docID, fmt.Sprint(h["tabId"]))[id] {
				t.Errorf("the citation's target %q is not live", id)
			}
		})
	}
}

// TestDryRunPlansTheRelink: a dry run has nothing to read back, so its hosted
// anchors get placeholder ids — but a rewrite re-mints them for real, and a dump
// whose placeholder never moved would show a run with no re-linking in it and
// understate what a real publish does.
func TestDryRunPlansTheRelink(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	var dump strings.Builder
	be, err := gdocs.New(context.Background(), gdocs.Config{
		DriveID: testDriveID, Bundle: "testbundle", Selection: "concepts",
		DocsEndpoint: srv.URL, DriveEndpoint: srv.URL, HTTPClient: &http.Client{},
		DryRunWriter: &dump,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := pipeline.Run(context.Background(), be, bundletest.Load(t, lateGlossaryBundle(300))); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.batchUpdates != 0 {
		t.Errorf("a dry run issued %d batchUpdate(s)", fake.batchUpdates)
	}
	// The citation is linked once against the placeholder the first write minted,
	// and again against the one the rewrite minted. Two DISTINCT placeholder
	// targets in the dump is exactly the re-link a real run would issue; one would
	// mean the dump modelled the rewrite as leaving its ids alone.
	targets := map[string]bool{}
	for _, id := range headingTargets(dump.String()) {
		targets[id] = true
	}
	if len(targets) < 2 {
		t.Errorf("the dump plans no re-link: heading targets = %v", targets)
	}
	for id := range targets {
		if !strings.HasPrefix(id, gdocs.DryRunHeadingID) {
			t.Errorf("a dry run planned a link to a non-placeholder target %q", id)
		}
	}
}

// headingTargets pulls the id of every heading link out of a dry-run dump.
func headingTargets(dump string) []string {
	var out []string
	for _, chunk := range strings.Split(dump, `"heading"`)[1:] {
		if id := between(chunk, `"id": "`, `"`); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func countWrites(log []string, want string) int {
	n := 0
	for _, w := range log {
		if w == want {
			n++
		}
	}
	return n
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestSortOrderThatAlreadyWorkedStillDoes: when the hosting file sorts BEFORE
// the citing page, its rewrites all land first and nothing was ever stranded.
// That case must stay correct AND stay cheap — no patch traffic.
func TestSortOrderThatAlreadyWorkedStillDoes(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	files := lateGlossaryBundle(300)
	files["okf.toml"] = "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n"
	files["CONTEXT.md"] = files["zz-glossary.md"]
	delete(files, "zz-glossary.md")
	files["alpha.md"] = "---\nokf_version: \"0.1\"\ntitle: Alpha\ntype: concept\n---\n\n" +
		"# Alpha\n\nAlpha cites [Term 0](/CONTEXT.md#term-0).\n"

	be := newBackend(t, srv.URL)
	if _, err := pipeline.Run(context.Background(), be, bundletest.Load(t, files)); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertNoStaleLinks(t, fake, be.DocumentID())
	// The fixture has no self-citations, so a link-only batch here could only be a
	// re-link — and nothing moved, so there should be none. The citing tab's link
	// must also have been applied exactly once.
	if fake.patchBatches != 0 {
		t.Errorf("nothing was stranded, so nothing should have been re-linked; got %d patch batch(es)",
			fake.patchBatches)
	}
	if n := countWrites(fake.writeLog, "link:Alpha"); n != 1 {
		t.Errorf("the citation was linked %d time(s), want exactly 1", n)
	}
}
