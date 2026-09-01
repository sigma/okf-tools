package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/backend/fake"
	"github.com/sigma/okf-tools/internal/publish/graph"
)

// loadBundle materializes an in-memory file set as a real okf bundle on disk and
// loads it through the production discover/load path, so Run drives Stage 1 against
// genuine parsed input.
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

// smallBundle is a representative bundle: nested indexes (parent-before-child), a
// cross-document link (content-refs-node), and a glossary hosting a cited anchor
// (content-refs-anchor) — every edge cause the transport must sequence.
func smallBundle() map[string]string {
	return map[string]string{
		"okf.toml":          "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md":          "---\nokf_version: \"0.1\"\n---\n# Root\n",
		"docs/adr/index.md": "# ADRs\n",
		"docs/adr/a.md":     "---\ntype: adr\ntitle: A\n---\nSee [B](b.md) and the [root KEK](../../CONTEXT.md#root-kek).\n",
		"docs/adr/b.md":     "---\ntype: adr\ntitle: B\n---\nSee [A](a.md).\n",
		"CONTEXT.md":        "# Glossary\n\n**Root KEK**: the root key-encryption key.\n",
	}
}

// scanAfterPublish reconstructs the CurrentState a real backend's Scanner would
// return after the whole bundle has been mirrored: every source node exists (with a
// synthetic backend id) and carries its expected content hash. Feeding this as the
// seed of a fresh run is exactly the steady-state re-run the near-noop property is
// about.
func scanAfterPublish(b *bundle.Bundle) *publish.CurrentState {
	nodes := map[publish.SymbolicID]publish.BackendID{}
	hashes := map[publish.SymbolicID]publish.Hash{}
	propHashes := map[publish.SymbolicID]publish.Hash{}
	for _, d := range b.Docs {
		id := publish.SymbolicID("node:" + d.Rel)
		nodes[id] = publish.BackendID("existing-" + d.Rel)
		hashes[id] = graph.ContentHash(d)
		propHashes[id] = graph.PropertyHash(d)
	}
	return publish.NewCurrentStateWithProps(nodes, hashes, propHashes, nil)
}

// TestRunPublishesEverythingAgainstFake: a first publish against an empty scan
// wires all three stages and mirrors the whole bundle into the fake store —
// executing one transaction per PackedTxn and resolving every source node and the
// cited anchor.
func TestRunPublishesEverythingAgainstFake(t *testing.T) {
	b := loadBundle(t, smallBundle())
	// maxCount=2 dials packing pressure so a mutual link (a <-> b, both new) stays
	// acyclic: creates seal separately from content.
	be := fake.New(fake.WithMaxCount(2))

	res, err := Run(context.Background(), be, b)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.TxnCount == 0 {
		t.Fatal("first publish should execute transactions, got 0")
	}
	if got := len(be.Executed()); got != res.TxnCount {
		t.Errorf("executed %d transactions, want one per PackedTxn (%d)", got, res.TxnCount)
	}
	for _, d := range b.Docs {
		id := publish.SymbolicID("node:" + d.Rel)
		if _, ok := res.Nodes[id]; !ok {
			t.Errorf("node %s never resolved", id)
		}
	}
	if _, ok := res.Anchors["glossary/root-kek"]; !ok {
		t.Errorf("anchor glossary/root-kek never resolved, anchors=%v", res.Anchors)
	}
}

// markerBundle designates the glossary/anchor host through the areas.json role
// marker (role: glossary) rather than an okf.toml [glossary] files list — the #156
// contract replacing the hardwired CONTEXT.md literal. okf.toml only flips the
// master opt-in; areas.json alone names the host.
func markerBundle() map[string]string {
	return map[string]string{
		"okf.toml": "[glossary]\nenabled = true\n",
		"areas.json": `{
			"docs":     {"directory": "docs", "type": "adr"},
			"glossary": {"file": "CONTEXT.md", "type": "glossary", "role": "glossary"}
		}`,
		"index.md":      "---\nokf_version: \"0.1\"\n---\n# Root\n",
		"docs/index.md": "# Docs\n",
		"docs/a.md":     "---\ntype: adr\ntitle: A\n---\nCite the [root KEK](../CONTEXT.md#root-kek).\n",
		"CONTEXT.md":    "# Glossary\n\n**Root KEK**: the root key-encryption key.\n",
	}
}

// TestRunHostsAnchorFromAreasMarker proves the areas.json glossary-role marker —
// not a filename convention — drives the publish end to end: CONTEXT.md is hosted as
// the anchor page purely because areas.json marks it role: glossary, and the anchor a
// page cites resolves through the pipeline.
func TestRunHostsAnchorFromAreasMarker(t *testing.T) {
	b := loadBundle(t, markerBundle())

	// Sanity: the marker (not an okf.toml files list) designated CONTEXT.md.
	if b.Areas == nil {
		t.Fatal("areas.json not loaded into the bundle")
	}
	if host, ok := b.Areas.GlossaryFile(); !ok || host != "CONTEXT.md" {
		t.Fatalf("areas marker host = %q,%v, want CONTEXT.md", host, ok)
	}
	if len(b.Glossaries) != 1 || b.Glossaries[0].Rel != "CONTEXT.md" {
		t.Fatalf("marker did not designate CONTEXT.md as the glossary: %v", b.Glossaries)
	}

	res, err := Run(context.Background(), fake.New(fake.WithMaxCount(2)), b)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := res.Anchors["glossary/root-kek"]; !ok {
		t.Errorf("anchor glossary/root-kek (hosted via the areas marker) never resolved, anchors=%v", res.Anchors)
	}
}

// TestRunNearNoopRerun is the headline acceptance property: an immediate re-run
// against a scan that already reflects the published state hash-skips every page,
// so the pipeline emits an empty transaction-DAG and makes ZERO backend calls.
func TestRunNearNoopRerun(t *testing.T) {
	b := loadBundle(t, smallBundle())
	seed := scanAfterPublish(b)
	be := fake.New(fake.WithScan(seed))

	res, err := Run(context.Background(), be, b)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.TxnCount != 0 {
		t.Errorf("near-noop re-run should emit no transactions, got %d", res.TxnCount)
	}
	if got := len(be.Executed()); got != 0 {
		t.Errorf("near-noop re-run should make zero backend calls, got %d executed", got)
	}
	if len(res.Nodes) != 0 || len(res.Anchors) != 0 {
		t.Errorf("near-noop re-run should mint nothing, got nodes=%v anchors=%v", res.Nodes, res.Anchors)
	}
}

// TestRunNearNoopSingleChange: after one page changes, only that page's work is
// emitted — the re-run is proportional to the change, not to bundle size. This is
// the "near" in near-noop (an edgeless graph for the untouched pages).
func TestRunNearNoopSingleChange(t *testing.T) {
	b := loadBundle(t, smallBundle())
	seed := scanAfterPublish(b)

	// Drift b.md's stored hash so only it is "changed"; every other page still
	// hash-matches and is skipped.
	changed := publish.SymbolicID("node:docs/adr/b.md")
	nodes := map[publish.SymbolicID]publish.BackendID{}
	hashes := map[publish.SymbolicID]publish.Hash{}
	propHashes := map[publish.SymbolicID]publish.Hash{}
	for id := range seed.Nodes() {
		nodes[id], _ = seed.NodeID(id)
		h, _ := seed.ContentHash(id)
		if id == changed {
			h = "stale-hash"
		}
		hashes[id] = h
		// Carry the matching property hash so only b.md's content drifts — every
		// other page still hash-skips both arms.
		propHashes[id], _ = seed.PropertyHash(id)
	}
	be := fake.New(fake.WithScan(publish.NewCurrentStateWithProps(nodes, hashes, propHashes, nil)))

	res, err := Run(context.Background(), be, b)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Only b.md re-asserts (SetProperties + SetContent, one block) — a handful of
	// transactions, far fewer than a full publish.
	if res.TxnCount == 0 {
		t.Fatal("a changed page should emit at least one transaction")
	}
	if res.TxnCount >= len(b.Docs) {
		t.Errorf("a single-page change emitted %d txns, expected far fewer than a full publish", res.TxnCount)
	}
}

// TestRunBannerThreadsIntoChangeDetection proves WithBanner reaches Stage 1: the
// banner is folded into the expected content hash, so a scan seeded with the plain
// (pre-banner) ContentHash — a near-noop without a banner — instead re-asserts every
// page when a banner is passed. This exercises the pipeline→graph wiring without
// reaching into the fake's opaque transaction internals.
func TestRunBannerThreadsIntoChangeDetection(t *testing.T) {
	b := loadBundle(t, smallBundle())
	seed := scanAfterPublish(b) // steady-state scan carrying the plain ContentHash

	// Baseline: without a banner, this seed is a near-noop.
	beNoBanner := fake.New(fake.WithScan(seed))
	res, err := Run(context.Background(), beNoBanner, b)
	if err != nil {
		t.Fatalf("Run (no banner): %v", err)
	}
	if res.TxnCount != 0 {
		t.Fatalf("no-banner steady-state should be a near-noop, got %d txns", res.TxnCount)
	}

	// With a banner, the expected hash is banner-folded and no longer matches the
	// plain-hash seed, so pages re-assert — the wiring is live.
	beBanner := fake.New(fake.WithScan(seed))
	bn := &graph.Banner{Text: "GEN", BaseURL: "https://h/r", Ref: "main"}
	res2, err := Run(context.Background(), beBanner, b, WithBanner(bn))
	if err != nil {
		t.Fatalf("Run (banner): %v", err)
	}
	if res2.TxnCount == 0 {
		t.Fatal("a banner over a plain-hash seed should re-publish, got 0 txns")
	}
}

// --- request accounting (#134) ----------------------------------------------

// meteredBackend is a fake that also meters its traffic — the optional
// backend.RequestReporter role the Notion backend implements for real.
type meteredBackend struct {
	*fake.Backend
	stats publish.RequestStats
}

func (m *meteredBackend) RequestStats() publish.RequestStats { return m.stats }

// A backend that meters its API traffic has that traffic reported in the Result,
// so the bin can print it alongside the transaction count.
func TestRunReportsBackendRequestStats(t *testing.T) {
	b := loadBundle(t, smallBundle())
	want := publish.RequestStats{Requests: 41, Throttled: 2, Transient: 1}
	be := &meteredBackend{Backend: fake.New(fake.WithScan(scanAfterPublish(b))), stats: want}

	res, err := Run(context.Background(), be, b)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stats != want || !res.Metered {
		t.Errorf("Stats = %+v (metered=%v), want %+v metered", res.Stats, res.Metered, want)
	}
}

// A backend that meters nothing (the fake, the filesystem export) leaves the
// stats at zero rather than making the Result unusable.
func TestRunWithoutRequestReporterReportsZeroStats(t *testing.T) {
	b := loadBundle(t, smallBundle())
	res, err := Run(context.Background(), fake.New(), b)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stats != (publish.RequestStats{}) || res.Metered {
		t.Errorf("Stats = %+v (metered=%v), want zeros and unmetered for a backend that issues no requests",
			res.Stats, res.Metered)
	}
}

// The operator's pacing lever reaches the backend: a configured interval selects a
// Notion backend that meters traffic, and an unset one leaves the default in place.
func TestSelectBackendAcceptsAPacingInterval(t *testing.T) {
	for _, d := range []*time.Duration{nil, ptr(0 * time.Second), ptr(50 * time.Millisecond), ptr(-1 * time.Second)} {
		be, err := SelectBackend(BackendNotion, &Config{NotionToken: "tok", NotionDBID: "ds1", NotionInterval: d})
		if err != nil {
			t.Fatalf("SelectBackend(interval=%v): %v", d, err)
		}
		if _, ok := be.(backend.RequestReporter); !ok {
			t.Errorf("interval=%v: notion backend must meter its traffic", d)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// An explicitly configured zero is passed to the backend rather than mistaken for
// "unset": the two readings differ, and only one matches the backend's contract
// that a non-positive interval paces nothing. This is the `--interval 0` trap.
func TestZeroIntervalIsExplicitNotUnset(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     *Config
		want    time.Duration
		wantSet bool
	}{
		{"unset leaves the backend default", &Config{}, 0, false},
		{"zero disables pacing", &Config{NotionInterval: ptr(0 * time.Second)}, 0, true},
		{"a duration paces at it", &Config{NotionInterval: ptr(50 * time.Millisecond)}, 50 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := intervalOverride(tc.cfg)
			if got != tc.want || ok != tc.wantSet {
				t.Errorf("intervalOverride = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.wantSet)
			}
		})
	}
}
