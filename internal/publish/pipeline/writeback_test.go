package pipeline

import (
	"context"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/backend/fake"
)

// TestRunScanModeDefaultsToStored: a plain Run scans in the cheap steady-state
// ScanStored mode; the opt-in WithScanMode(ScanRecompute) reaches the scanner.
func TestRunScanModeDefaultsToStored(t *testing.T) {
	b := loadBundle(t, smallBundle())

	def := fake.New(fake.WithScan(scanAfterPublish(b)))
	if _, err := Run(context.Background(), def, b); err != nil {
		t.Fatalf("Run (default): %v", err)
	}
	if mode, ok := def.LastScanMode(); !ok || mode != backend.ScanStored {
		t.Errorf("default scan mode = %v (called=%v), want ScanStored", mode, ok)
	}

	rec := fake.New(fake.WithScan(scanAfterPublish(b)))
	if _, err := Run(context.Background(), rec, b,
		WithScanMode(backend.ScanRecompute)); err != nil {
		t.Fatalf("Run (recompute): %v", err)
	}
	if mode, ok := rec.LastScanMode(); !ok || mode != backend.ScanRecompute {
		t.Errorf("opt-in scan mode = %v (called=%v), want ScanRecompute", mode, ok)
	}
}

// TestRunWritesBackEveryPublishedNode: a first full publish records write-back
// provenance for every source node — with the glossary node carrying its hosted
// anchor id — so the next ScanStored would read current state.
func TestRunWritesBackEveryPublishedNode(t *testing.T) {
	b := loadBundle(t, smallBundle())
	be := fake.New(fake.WithMaxCount(2))

	if _, err := Run(context.Background(), be, b); err != nil {
		t.Fatalf("Run: %v", err)
	}

	written := be.WrittenBack()
	if len(written) != 1 {
		t.Fatalf("want one write-back for the publish, got %d", len(written))
	}
	nodes := written[0].Nodes
	for _, d := range b.Docs {
		id := publish.SymbolicID("node:" + d.Rel)
		np, ok := nodes[id]
		if !ok {
			t.Errorf("write-back missing provenance for %s", id)
			continue
		}
		if np.ID == "" || np.Hash == "" {
			t.Errorf("provenance for %s = %+v, want a resolved id and hash", id, np)
		}
	}
	if g, ok := nodes["node:CONTEXT.md"]; !ok || len(g.Anchors) == 0 {
		t.Errorf("glossary node write-back should carry its hosted anchor, got %+v", g)
	}
}

// TestRunNearNoopWritesNothingBack: the steady-state unchanged re-run records no
// write-back — self-description stays a true no-op when nothing changed.
func TestRunNearNoopWritesNothingBack(t *testing.T) {
	b := loadBundle(t, smallBundle())
	be := fake.New(fake.WithScan(scanAfterPublish(b)))

	if _, err := Run(context.Background(), be, b); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := be.WrittenBack(); len(got) != 0 {
		t.Errorf("near-noop re-run should write nothing back, got %d write-backs", len(got))
	}
}
