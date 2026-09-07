// Package backendtest is the cross-backend conformance kit: one set of
// behavioural properties that EVERY publishing backend must satisfy, run against
// each backend's own harness.
//
// It exists because the seam's backends were each tested only against their own
// fixtures, in their own package, with their own fake. Nothing published the same
// bundle through more than one of them, so a property they were all assumed to
// have could break in exactly one and no test noticed. That is not hypothetical:
// a glossary citing its own terms published cleanly through two backends and
// failed on every selection through the third for that backend's whole lifetime
// (#170), and a second bug of the same class followed immediately (#171).
//
// The dependency is INVERTED on purpose. The kit knows nothing about any concrete
// backend — it imports none of them, and never will — and each backend's own test
// package calls Run with a factory that builds the backend against whatever fake
// or temp directory that package already has. Lifting the fakes into a shared
// package would have been the other direction: ~1400 lines of test server moved
// into packages that ship, to serve a test. This is the shape fstest.TestFS and
// the database/sql driver kits use.
//
// Adding a backend means calling Run and, where the backend can report them,
// supplying the optional probes below. Everything the kit covers is then covered
// for that backend from its first day.
//
// What it can REACH is bounded, and worth stating so the coverage is not
// overread. The kit asserts on a run's resolution result — the ids a publish
// produced — because that is the only currency every medium shares. #170 lives
// there and is caught: the publish itself failed. #171 does NOT: a link left
// pointing at a re-minted heading id is a fact about bytes inside a Google Doc,
// the resolution result self-heals, and no medium-neutral assertion can see it.
// That class stays the property of the backend's own suite, and the kit does not
// pretend otherwise.
//
// The kit publishes through pipeline.Run — the path production uses — rather than
// wiring the stages itself, so a property proved here is proved about the real
// thing. The cost is that the kit reaches the pipeline, which knows every
// concrete backend, so a caller must live in an EXTERNAL test package or close an
// import cycle. Two of the three real backends already did; the third needed a
// one-function bridge and no fake moved.
package backendtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/sigma/okf-tools/internal/areas"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/pipeline"
)

// Subject is one backend, freshly built, plus the optional probes the kit uses to
// gate the properties that cannot be expressed against every medium.
//
// A probe is nil when the backend cannot answer it. The kit then SKIPS the
// property that needs it, visibly, rather than passing it vacuously — a silent
// pass is the failure mode this package exists to prevent.
type Subject struct {
	// Backend is the backend under test, ready to publish: constructed, pointed at
	// its fake, and provisioned if it needs to be.
	Backend backend.Backend

	// Writes reports how many write requests the backend has issued so far. It
	// gates the near-noop property.
	//
	// What counts as a write is MEDIUM-SPECIFIC, which is the reason this probe
	// belongs to the caller rather than the kit: Notion's query endpoint is a POST
	// and reads, so "every request that is not a GET" would count a read as a write
	// and fail a backend that is behaving.
	//
	// Set it only when the backend's own Scan reports what it previously wrote,
	// because that is what near-noop actually rests on: the scan finds the prior
	// state, the hashes match, and nothing is rewritten. A backend whose Scan is
	// canned or empty rewrites everything on every run BY DESIGN, and holding it to
	// this property would assert a bug that is not there. Leave it nil there, and
	// say why at the call site.
	Writes func() int

	// Snapshot reports the published output as a comparable string. It gates the
	// determinism property, which is how a backend whose destination is a tree of
	// files expresses what a write count expresses for a remote API. Leave it nil
	// when the destination cannot be read back cheaply.
	Snapshot func() string
}

// Factory builds a fresh Subject for one property. It is called once per
// property, never reused: a property that ran against a destination another
// property already wrote to would be testing the wrong thing.
//
// It takes no fixture. An earlier version passed the files in, which read as a
// convenience and was a trap: the kit writes the bundle into its own temp
// directory, so a factory deriving a destination from that map would aim at a
// tree the pipeline never reads.
type Factory func(t *testing.T) Subject

// Fixture is one bundle every backend publishes, with what the kit expects to be
// true of the result regardless of medium.
type Fixture struct {
	// Name is the subtest name.
	Name string
	// Why records what this fixture is here to catch, so a failure is diagnosable
	// without reading the fixture.
	Why string
	// Files is the bundle, as path → content.
	Files map[string]string
	// Nodes are bundle-relative paths every backend must publish. Ranging over
	// only what a run RETURNED would pass a backend that dropped a page silently,
	// so the expectation is stated rather than derived from the result.
	//
	// An area's landing README is deliberately absent: the backends disagree about
	// publishing it (#163), and that disagreement has its own property below.
	Nodes []string
}

// Fixtures are the bundles the kit publishes through every backend. They are
// owned here rather than by each caller: the whole point is that every backend
// sees the SAME input.
func Fixtures() []Fixture {
	return []Fixture{
		{
			Name:  "self-citing-glossary",
			Nodes: []string{"index.md", "CONTEXT.md"},
			Why: "a term defined in terms of another term in the same file — the " +
				"normal shape of a glossary, and the one that made a whole backend " +
				"unusable (#170)",
			Files: map[string]string{
				"okf.toml": "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
				"index.md": frontmatter("Index", "index") + "# Index\n\nThe root page.\n",
				"CONTEXT.md": frontmatter("Context", "context") + "# Keys\n\n" +
					"- **Widget**: a small thing, held by a [Sprocket](/CONTEXT.md#sprocket).\n" +
					"- **Sprocket**: a toothed thing.\n",
			},
		},
		{
			Name:  "cross-page-citation",
			Nodes: []string{"index.md", "CONTEXT.md", "alpha.md", "beta.md"},
			Why:   "a page citing a term another page hosts — the ordinary late-bound reference",
			Files: map[string]string{
				"okf.toml": "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
				"index.md": frontmatter("Index", "index") + "# Index\n\n- [Alpha](/alpha.md)\n",
				"CONTEXT.md": frontmatter("Context", "context") + "# Keys\n\n" +
					"- **Widget**: a small thing.\n",
				"alpha.md": frontmatter("Alpha", "concept") + "# Alpha\n\n" +
					"Alpha cites a [Widget](/CONTEXT.md#widget) and links [Beta](/beta.md).\n",
				"beta.md": frontmatter("Beta", "concept") + "# Beta\n\nBeta stands alone.\n",
			},
		},
		{
			Name: "area-with-landing-readme",
			// No index.md: areas.json declares the export scope, so a page outside
			// every declared area is legitimately not published. The fixture omits it
			// rather than carrying a file no backend is expected to write.
			Nodes: []string{"CONTEXT.md", "concepts/alpha.md"},
			Why: "a DECLARED area whose landing README exists. The backends disagree about " +
				"publishing it (#163), so the kit asserts the disagreement is honest rather " +
				"than picking a side",
			Files: map[string]string{
				// areas.json is what makes concepts/ an area at all. Without it the
				// README is an ordinary page and this fixture tests nothing it claims.
				"areas.json": `{"concepts": {"directory": "concepts", "type": "concept"},` +
					` "context": {"file": "CONTEXT.md", "type": "context", "role": "glossary"}}`,
				"okf.toml": "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
				"CONTEXT.md": frontmatter("Context", "context") + "# Keys\n\n" +
					"- **Widget**: a small thing.\n",
				"concepts/README.md": frontmatter("Concepts Overview", "concept") +
					"# Concepts\n\nWhat this area covers.\n",
				"concepts/alpha.md": frontmatter("Alpha", "concept") +
					"# Alpha\n\nA [Widget](/CONTEXT.md#widget).\n",
			},
		},
	}
}

func frontmatter(title, kind string) string {
	return "---\nokf_version: \"0.1\"\ntitle: " + title + "\ntype: " + kind + "\n---\n\n"
}

// Run executes the whole kit against one backend. Each property is a named
// subtest, so a failure names the backend, the fixture and the property rather
// than one opaque "conformance" failure.
func Run(t *testing.T, newSubject Factory) {
	t.Helper()
	for _, f := range Fixtures() {
		t.Run(f.Name, func(t *testing.T) {
			runFixture(t, newSubject, f)
		})
	}
}

func runFixture(t *testing.T, newSubject Factory, f Fixture) {
	t.Helper()

	// Every property gets its own subject and its own run: they assert different
	// things about the same publish, but a shared destination would let one
	// property's writes decide another's outcome.
	t.Run("publishes", func(t *testing.T) {
		res := publishFixture(t, newSubject, f)
		if len(res.Nodes) == 0 {
			t.Errorf("the run published no nodes at all (%s)", f.Why)
		}
	})

	t.Run("every declared anchor resolves", func(t *testing.T) {
		b := loadBundle(t, f.Files)
		res := publishBundle(t, newSubject, f, b)
		// The expectation is DERIVED from the bundle, not listed here: a
		// hand-written list drifts silently the moment a fixture gains a term or a
		// heading, and quietly stops asserting the thing it names.
		want := declaredAnchors(b)
		if len(want) == 0 {
			t.Fatalf("the fixture declares no anchors, so this property asserts nothing")
		}
		for _, name := range want {
			id, ok := res.Anchors[name]
			switch {
			case !ok:
				t.Errorf("anchor %s never resolved; resolved: %v",
					name, anchorNames(res.Anchors))
			case id == "":
				t.Errorf("anchor %s resolved to an EMPTY id, which cites nothing", name)
			}
		}
	})

	t.Run("every expected node is published and resolves", func(t *testing.T) {
		res := publishFixture(t, newSubject, f)
		got := map[string]publish.BackendID{}
		for id, backendID := range res.Nodes {
			got[id.Rel()] = backendID
		}
		for _, rel := range f.Nodes {
			id, ok := got[rel]
			switch {
			case !ok:
				t.Errorf("%s was never published; published: %v", rel, relsOf(got))
			case id == "":
				t.Errorf("%s resolved to an empty backend id", rel)
			}
		}
	})

	t.Run("area roots match the backend's declaration", func(t *testing.T) {
		b := loadBundle(t, f.Files)
		roots := b.AreaRootDocs()
		if len(roots) == 0 {
			t.Skip("fixture declares no area roots")
		}
		s := newSubject(t)
		res := publishBundleWith(t, s, b, f)
		published := map[string]bool{}
		for id := range res.Nodes {
			published[id.Rel()] = true
		}
		// Neither answer is "correct" — a document opens with its area's overview
		// and a database of pages does not (#163). What must hold is that the
		// backend does what it SAYS.
		//
		// The reach here is narrow, and worth stating so it is not overread:
		// generation consumes this same declaration when it decides which docs
		// enter the graph, so a merely MISLABELLED backend stays self-consistent
		// and passes. What this catches is the gap after that decision — a backend
		// that is handed its area root and then fails to publish it, which the
		// expected-node property cannot cover because the two backends
		// legitimately disagree about whether the root belongs there at all.
		want := PublishesAreaRoots(s.Backend)
		for _, root := range roots {
			if got := published[root.Rel]; got != want {
				t.Errorf("PublishesAreaRoots() = %v, but %s published = %v", want, root.Rel, got)
			}
		}
	})

	t.Run("republish is a near-noop", func(t *testing.T) {
		s := newSubject(t)
		if s.Writes == nil {
			t.Skip("backend does not count its writes; see the determinism property instead")
		}
		before, after := republish(t, s, f, func() string { return fmt.Sprint(s.Writes()) })
		if before != after {
			t.Errorf("republishing unchanged content took the write count from %s "+
				"to %s; an unchanged run must write nothing", before, after)
		}
	})

	t.Run("a forced republish rewrites every node", func(t *testing.T) {
		// The escape hatch has to work on EVERY backend, not just the one it was
		// reported against: a rendering fix that cannot reach a live destination is
		// a fix that silently did not ship (#183). An unchanged re-run publishes
		// nothing; the same re-run forced must publish the whole fixture.
		s := newSubject(t)
		b := loadBundle(t, f.Files)
		publishBundleWith(t, s, b, f)

		steady, err := pipeline.Run(context.Background(), s.Backend, b)
		if err != nil {
			t.Fatalf("steady re-run failed: %v", err)
		}
		if steady.TxnCount != 0 {
			t.Skipf("this backend's unchanged re-run is not a noop (%d txns), so a forced one proves nothing",
				steady.TxnCount)
		}

		forced, err := pipeline.Run(context.Background(), s.Backend, b, pipeline.WithForceRewrite())
		if err != nil {
			t.Fatalf("forced re-run failed: %v", err)
		}
		if forced.TxnCount == 0 {
			t.Errorf("--force published nothing (%s)", f.Why)
		}
	})

	t.Run("republish is deterministic", func(t *testing.T) {
		s := newSubject(t)
		if s.Snapshot == nil {
			t.Skip("backend cannot snapshot its destination; see the near-noop property instead")
		}
		before, after := republish(t, s, f, s.Snapshot)
		if before != after {
			t.Errorf("republishing unchanged content changed the destination:\n"+
				"--- first\n%s\n--- second\n%s", before, after)
		}
	})
}

// republish publishes the fixture twice through one subject and reports what
// capture saw after each run. Both republish properties are the same experiment
// read through a different probe.
func republish(t *testing.T, s Subject, f Fixture,
	capture func() string) (before, after string) {
	t.Helper()
	b := loadBundle(t, f.Files)
	publishBundleWith(t, s, b, f)
	before = capture()
	publishBundleWith(t, s, b, f)
	return before, capture()
}

// publishFixture builds a fresh subject and publishes the fixture through it.
func publishFixture(t *testing.T, newSubject Factory, f Fixture) *pipeline.Result {
	t.Helper()
	return publishBundle(t, newSubject, f, loadBundle(t, f.Files))
}

func publishBundle(t *testing.T, newSubject Factory, f Fixture,
	b *bundle.Bundle) *pipeline.Result {
	t.Helper()
	return publishBundleWith(t, newSubject(t), b, f)
}

// publishBundleWith runs one publish and fails the test if it errored — the
// property every other property depends on.
func publishBundleWith(t *testing.T, s Subject, b *bundle.Bundle,
	f Fixture) *pipeline.Result {
	t.Helper()
	res, err := pipeline.Run(context.Background(), s.Backend, b)
	if err != nil {
		t.Fatalf("publish failed: %v\nfixture is here to catch: %s", err, f.Why)
	}
	return res
}

// declaredAnchors is every anchor the bundle's glossary defines — a term or a
// heading slug, namespaced by the glossary role, which is how the graph mints
// them.
func declaredAnchors(b *bundle.Bundle) []publish.AnchorName {
	var out []publish.AnchorName
	for _, d := range b.Docs {
		for _, a := range d.Anchors {
			out = append(out, publish.AnchorName(areas.RoleGlossary+"/"+a.Slug))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func relsOf(m map[string]publish.BackendID) []string {
	out := make([]string, 0, len(m))
	for rel := range m {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

// loadBundle writes a fixture to a temp directory and loads it, which is what
// every backend's own suite was doing separately.
func loadBundle(t *testing.T, files map[string]string) *bundle.Bundle {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, cfgPath, err := bundle.Discover(dir, "", "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	b, err := bundle.Load(root, cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return b
}

func anchorNames(m map[publish.AnchorName]publish.BackendID) []publish.AnchorName {
	out := make([]publish.AnchorName, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	return out
}

// PublishesAreaRoots reports what a backend does with an area's landing README,
// which the backends deliberately disagree about (#163). The kit does not assert
// either answer — it is here so a caller's own suite can, and so the difference
// is visible as a declaration rather than looking like a conformance failure.
func PublishesAreaRoots(be backend.Backend) bool {
	p, ok := be.(graph.AreaRootPublisher)
	return ok && p.PublishesAreaRoots()
}
