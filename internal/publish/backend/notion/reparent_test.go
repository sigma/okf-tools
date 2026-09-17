package notion

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle/bundletest"
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// The scan reports where each node is RECORDED: a row records itself, a subtree
// member is recorded by its row — the fact generation needs to see a node whose
// placement changed (sigma/okf-tools#209).
func TestScanStoredReportsOwners(t *testing.T) {
	f := newFakeNotion()
	f.rows = []map[string]any{
		row("page-a", map[string]any{"path": richProp("dir/a.md"), "hash": richProp("hA")}),
		row("page-readme", map[string]any{
			"path": richProp("dir/README.md"),
			"hashes": richProp(mustJSON(t, map[string]subtreeEntry{
				"dir/b.md": {ID: "page-b", Hash: "hB"},
			})),
		}),
	}
	be := newServer(t, f)

	cs, err := be.Scan(context.Background(), backend.ScanStored)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if o, ok := cs.Owner("node:dir/a.md"); !ok || o != "" {
		t.Errorf("Owner(a.md) = (%q,%v), want (\"\",true): a row records itself", o, ok)
	}
	if o, ok := cs.Owner("node:dir/README.md"); !ok || o != "" {
		t.Errorf("Owner(README) = (%q,%v), want (\"\",true)", o, ok)
	}
	if o, ok := cs.Owner("node:dir/b.md"); !ok || o != "node:dir/README.md" {
		t.Errorf("Owner(b.md) = (%q,%v), want the README row", o, ok)
	}
}

// clusterFiles is a bundle whose docs area holds two pages, with or without the
// README that turns their directory into a cluster.
func clusterFiles(withReadme bool, aBody string) map[string]string {
	files := map[string]string{
		"okf.toml":       "",
		"areas.json":     `{"docs": {"directory": "docs", "type": "doc"}}`,
		"index.md":       "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"docs/README.md": "# Docs\n",
		"docs/dir/a.md":  "---\ntype: doc\ntitle: A\n---\n" + aBody,
		"docs/dir/b.md":  "---\ntype: doc\ntitle: B\n---\nBody of B.\n",
	}
	if withReadme {
		files["docs/dir/README.md"] = "---\ntype: doc\ntitle: Cluster\n---\nEntry point.\n"
	}
	return files
}

// rowPaths reports the `path` column of every live row, so a test can say which
// pages are rows.
func rowPaths(t *testing.T, f *fakeNotion) map[string]string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for _, r := range f.rows {
		props, _ := r["properties"].(map[string]any)
		out[servedText(t, props["path"])] = r["id"].(string)
	}
	return out
}

// subtreeOf decodes a row's `hashes` subtree map as the fake now holds it — the
// record the next scan will read.
func subtreeOf(t *testing.T, f *fakeNotion, rowID string) map[string]subtreeEntry {
	t.Helper()
	f.mu.Lock()
	stored := f.pageProps[rowID]["hashes"]
	f.mu.Unlock()
	if stored == nil {
		return map[string]subtreeEntry{}
	}
	m, err := storedSubtree(servedText(t, stored), rowID)
	if err != nil {
		t.Fatalf("decode subtree map of %s: %v", rowID, err)
	}
	return m
}

// The bug as reported: two rows published, then a README appears and one of them
// changes. Both rows are archived and re-created as subpages of the README — the
// unchanged sibling too — and the next scan claims each path exactly once.
func TestReadmeOverRowsReparentsTheCluster(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	if _, err := runPublish(t, be, bundletest.Load(t, clusterFiles(false, "Body of A.\n")), backend.ScanStored); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	before := rowPaths(t, f)
	if _, ok := before["docs/dir/a.md"]; !ok {
		t.Fatalf("a.md should start as a row; rows = %v", before)
	}

	if _, err := runPublish(t, be, bundletest.Load(t, clusterFiles(true, "Body of A, edited.\n")), backend.ScanStored); err != nil {
		t.Fatalf("second publish: %v", err)
	}

	after := rowPaths(t, f)
	for _, rel := range []string{"docs/dir/a.md", "docs/dir/b.md"} {
		if id, still := after[rel]; still {
			t.Errorf("%s is still a row (%s) after the README appeared", rel, id)
		}
	}
	readmeID, ok := after["docs/dir/README.md"]
	if !ok {
		t.Fatalf("README is not a row; rows = %v", after)
	}
	recorded := subtreeOf(t, f, readmeID)
	for _, rel := range []string{"docs/dir/a.md", "docs/dir/b.md"} {
		e, ok := recorded[rel]
		if !ok {
			t.Errorf("README row's map does not name %s: %v", rel, recorded)
		} else if e.ID == before[rel] {
			t.Errorf("README row's map names %s by its OLD row %s — the bug as reported", rel, e.ID)
		}
	}

	scan, err := be.Scan(context.Background(), backend.ScanStored)
	if err != nil {
		t.Fatalf("scan after re-parent: %v", err)
	}
	for _, rel := range []string{"docs/dir/a.md", "docs/dir/b.md"} {
		id, ok := scan.NodeID(publish.NodeRef(rel))
		if !ok {
			t.Errorf("%s vanished from the mirror's record", rel)
			continue
		}
		if id == publish.BackendID(before[rel]) {
			t.Errorf("%s still resolves to its old row %s; want a new subpage", rel, id)
		}
		if !f.childPages[string(id)] {
			t.Errorf("%s (%s) is not a child_page", rel, id)
		}
		if o, _ := scan.Owner(publish.NodeRef(rel)); o != publish.NodeRef("docs/dir/README.md") {
			t.Errorf("%s recorded by %q, want the README row %s", rel, o, readmeID)
		}
	}

	// And a third run over the same source is a steady state: nothing to re-parent.
	ops, err := runPublish(t, be, bundletest.Load(t, clusterFiles(true, "Body of A, edited.\n")), backend.ScanStored)
	if err != nil {
		t.Fatalf("third publish: %v", err)
	}
	if len(ops) != 0 {
		t.Errorf("steady state planned %d ops, want none", len(ops))
	}
}

// The reverse: the README goes away. Its row is archived (taking the subpages
// with it) and the pages come back as rows of their own.
func TestReadmeRemovedReparentsSubpagesToRows(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	if _, err := runPublish(t, be, bundletest.Load(t, clusterFiles(true, "Body of A.\n")), backend.ScanStored); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if _, ok := rowPaths(t, f)["docs/dir/a.md"]; ok {
		t.Fatalf("a.md should start as a subpage; rows = %v", rowPaths(t, f))
	}

	if _, err := runPublish(t, be, bundletest.Load(t, clusterFiles(false, "Body of A.\n")), backend.ScanStored); err != nil {
		t.Fatalf("second publish: %v", err)
	}

	after := rowPaths(t, f)
	if _, still := after["docs/dir/README.md"]; still {
		t.Errorf("README row survived its removal; rows = %v", after)
	}
	for _, rel := range []string{"docs/dir/a.md", "docs/dir/b.md"} {
		if _, ok := after[rel]; !ok {
			t.Errorf("%s did not come back as a row; rows = %v", rel, after)
		}
	}
	ops, err := runPublish(t, be, bundletest.Load(t, clusterFiles(false, "Body of A.\n")), backend.ScanStored)
	if err != nil {
		t.Fatalf("third publish: %v", err)
	}
	if len(ops) != 0 {
		t.Errorf("steady state planned %d ops, want none", len(ops))
	}
}

// servedText reads the plain text of a property as the fake SERVES it — the
// server-filled plain_text shape the scanner reads — by decoding it through the
// client's own property type. (plainTextOf reads the other direction: the
// {"text":{"content":…}} shape a client sends.)
func servedText(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-encode served property: %v", err)
	}
	var p property
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode served property: %v", err)
	}
	return plainText(p)
}

// The nested case (#141): a leaf moving from one cluster into a nested index of
// another is recorded on the NEW cluster's row — its nearest ancestor row, not its
// immediate parent — and dropped from the old one.
func TestReparentBetweenClustersRecordsOnTheNewRow(t *testing.T) {
	files := func(leafUnder string) map[string]string {
		return map[string]string{
			"okf.toml":              "",
			"areas.json":            `{"docs": {"directory": "docs", "type": "doc"}}`,
			"index.md":              "---\nokf_version: \"0.1\"\n---\nRoot.\n",
			"docs/README.md":        "# Docs\n",
			"docs/x/README.md":      "---\ntype: doc\ntitle: X\n---\nX.\n",
			"docs/y/README.md":      "---\ntype: doc\ntitle: Y\n---\nY.\n",
			"docs/y/deep/README.md": "---\ntype: doc\ntitle: Deep\n---\nDeep.\n",
			leafUnder + "/leaf.md":  "---\ntype: doc\ntitle: Leaf\n---\nLeaf.\n",
		}
	}
	f := newFakeNotion()
	be := newServer(t, f)

	if _, err := runPublish(t, be, bundletest.Load(t, files("docs/x")), backend.ScanStored); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	rows := rowPaths(t, f)
	xRow, yRow := rows["docs/x/README.md"], rows["docs/y/README.md"]
	if _, ok := subtreeOf(t, f, xRow)["docs/x/leaf.md"]; !ok {
		t.Fatalf("leaf should start recorded on x's row; x map = %v", subtreeOf(t, f, xRow))
	}

	if _, err := runPublish(t, be, bundletest.Load(t, files("docs/y/deep")), backend.ScanStored); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if _, still := subtreeOf(t, f, xRow)["docs/x/leaf.md"]; still {
		t.Errorf("x's row still records the leaf after it moved")
	}
	if _, ok := subtreeOf(t, f, yRow)["docs/y/deep/leaf.md"]; !ok {
		t.Errorf("y's row does not record the moved leaf; y map = %v", subtreeOf(t, f, yRow))
	}
	if _, err := be.Scan(context.Background(), backend.ScanStored); err != nil {
		t.Fatalf("scan after move: %v", err)
	}
}
