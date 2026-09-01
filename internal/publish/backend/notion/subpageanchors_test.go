package notion

import (
	"context"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// subpageGlossaryFiles is a bundle whose glossary is a SUBPAGE: a root index sits
// above it, so CONTEXT.md is page-parented and has no row of its own. A citing page
// links into it, which is what makes the missing anchor map fatal.
func subpageGlossaryFiles() map[string]string {
	return map[string]string{
		"okf.toml":   "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md":   "---\nokf_version: \"0.1\"\n---\n# Root\n",
		"CONTEXT.md": "# Glossary\n\n**Root KEK**: the root key-encryption key.\n\n**Envoy**: the carrier.\n",
		"a.md":       "---\ntype: adr\ntitle: A\n---\nThe [root KEK](CONTEXT.md#root-kek) protects the [envoy](CONTEXT.md#envoy).\n",
	}
}

func subpageGlossaryBundle(t *testing.T) *bundle.Bundle {
	return loadBundleFiles(t, subpageGlossaryFiles())
}

// A glossary hosted as a subpage records its anchors — in its entry in the owning
// row's subtree map, since it has no row of its own to carry an anchors column.
func TestSubpageGlossaryRecordsItsAnchors(t *testing.T) {
	b := subpageGlossaryBundle(t)
	f := newFakeNotion()
	be := newServer(t, f)

	if _, err := runPublish(t, be, b, backend.ScanStored); err != nil {
		t.Fatalf("publish: %v", err)
	}

	entry, ok := storedMapOf(t, f, theOnlyRow(t, f))["CONTEXT.md"]
	if !ok {
		t.Fatal("the glossary subpage has no entry in the owning row's map")
	}
	for _, name := range []string{"glossary/root-kek", "glossary/envoy"} {
		if id := entry.Anchors[name]; id == "" {
			t.Errorf("anchor %s not recorded on the subpage's entry: %+v", name, entry)
		}
	}
}

// The regression this issue is about: with the anchors recorded, a later run seeds
// them, so editing a page that CITES the glossary publishes instead of deadlocking on
// a reference nothing can resolve.
func TestEditingAPageThatCitesASubpageGlossary(t *testing.T) {
	files := subpageGlossaryFiles()
	f := newFakeNotion()
	be := newServer(t, f)
	if _, err := runPublish(t, be, loadBundleFiles(t, files), backend.ScanStored); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	scan, err := be.Scan(context.Background(), backend.ScanStored)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, name := range []publish.AnchorName{"glossary/root-kek", "glossary/envoy"} {
		if _, ok := scan.AnchorID(name); !ok {
			t.Errorf("steady-state scan does not seed %s from the subpage's entry", name)
		}
	}

	// Edit the citing page only. The glossary hash-skips, so nothing in this run
	// produces its anchors: they must come from the seed.
	files["a.md"] = "---\ntype: adr\ntitle: A\n---\nThe [root KEK](CONTEXT.md#root-kek) guards the [envoy](CONTEXT.md#envoy), always.\n"
	if _, err := runPublish(t, be, loadBundleFiles(t, files), backend.ScanStored); err != nil {
		t.Fatalf("editing a citing page failed: %v", err)
	}
}

// A mirror recorded BEFORE this change has a subpage glossary and no anchors
// anywhere. The next run must converge rather than abort: the host is re-asserted
// because the anchors its content declares cannot be resolved from the seed.
func TestLegacyMirrorWithNoRecordedAnchorsConverges(t *testing.T) {
	files := subpageGlossaryFiles()
	f := newFakeNotion()
	be := newServer(t, f)
	if _, err := runPublish(t, be, loadBundleFiles(t, files), backend.ScanStored); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	// Strip the recorded anchors, leaving exactly what the old code would have left.
	rowID := theOnlyRow(t, f)
	stripped := storedMapOf(t, f, rowID)
	for subpath, e := range stripped {
		e.Anchors = nil
		stripped[subpath] = e
	}
	writeSubtreeMap(t, f, rowID, stripped)

	// Editing a citing page against that state must still publish.
	files["a.md"] = "---\ntype: adr\ntitle: A\n---\nThe [root KEK](CONTEXT.md#root-kek) and the [envoy](CONTEXT.md#envoy), revised.\n"
	if _, err := runPublish(t, be, loadBundleFiles(t, files), backend.ScanStored); err != nil {
		t.Fatalf("legacy state did not converge: %v", err)
	}
	entry := storedMapOf(t, f, rowID)["CONTEXT.md"]
	for _, name := range []string{"glossary/root-kek", "glossary/envoy"} {
		if entry.Anchors[name] == "" {
			t.Errorf("the repair did not re-record %s: %+v", name, entry)
		}
	}
}

// An unchanged re-run stays a near-noop, and the anchors survive both scan modes.
func TestSubpageAnchorsRoundTripThroughBothScanModes(t *testing.T) {
	b := subpageGlossaryBundle(t)
	f := newFakeNotion()
	be := newServer(t, f)
	if _, err := runPublish(t, be, b, backend.ScanStored); err != nil {
		t.Fatalf("publish: %v", err)
	}

	for _, mode := range []backend.ScanMode{backend.ScanStored, backend.ScanRecompute} {
		scan, err := be.Scan(context.Background(), mode)
		if err != nil {
			t.Fatalf("scan(%v): %v", mode, err)
		}
		for _, name := range []publish.AnchorName{"glossary/root-kek", "glossary/envoy"} {
			if _, ok := scan.AnchorID(name); !ok {
				t.Errorf("scan mode %v does not seed %s", mode, name)
			}
		}
		ops, err := runPublish(t, be, b, mode)
		if err != nil {
			t.Fatalf("re-run(%v): %v", mode, err)
		}
		if len(ops) != 0 {
			t.Errorf("re-run in mode %v planned %d op(s), want none", mode, len(ops))
		}
	}
}

// The #138 rule extends to a subpage host: a recorded anchor whose block is no longer
// a live child of that subpage is dangling, and the subpage is re-asserted.
func TestDanglingAnchorsOnASubpageHostForceReassertion(t *testing.T) {
	b := subpageGlossaryBundle(t)
	f := newFakeNotion()
	be := newServer(t, f)
	if _, err := runPublish(t, be, b, backend.ScanStored); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Break it the way an interrupted replace does: the recorded ids stop being live
	// children while the page's content still matches source.
	scan, err := be.Scan(context.Background(), backend.ScanStored)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	hostID, ok := scan.NodeID(publish.NodeRef("CONTEXT.md"))
	if !ok {
		t.Fatal("glossary subpage not in the mirror")
	}
	f.mu.Lock()
	for _, blk := range f.blocks[string(hostID)] {
		blk["id"] = "reborn-" + blk["id"].(string)
	}
	f.mu.Unlock()

	rec, err := be.Scan(context.Background(), backend.ScanRecompute)
	if err != nil {
		t.Fatalf("recompute scan: %v", err)
	}
	if _, ok := rec.ContentHash(publish.NodeRef("CONTEXT.md")); ok {
		t.Error("a subpage host with dangling anchors must withhold its content hash")
	}

	if _, err := runPublish(t, be, b, backend.ScanRecompute); err != nil {
		t.Fatalf("repair run: %v", err)
	}
	healed, err := be.Scan(context.Background(), backend.ScanRecompute)
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	live := liveChildIDs(t, f, string(hostID))
	for _, name := range []publish.AnchorName{"glossary/root-kek", "glossary/envoy"} {
		id, ok := healed.AnchorID(name)
		if !ok {
			t.Errorf("anchor %s unresolved after the repair", name)
			continue
		}
		if !live[string(id)] {
			t.Errorf("anchor %s resolves to %q, which is not a live child of the host", name, id)
		}
	}
}
