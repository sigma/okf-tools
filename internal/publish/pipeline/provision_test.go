package pipeline

import (
	"context"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/backend/fake"
	"github.com/sigma/okf-tools/internal/publish/transport"
)

// provisioningBackend wraps a real backend and records the order of its lifecycle
// calls, so a test can prove Run invokes the optional Provisioner role exactly
// once, before the scan.
type provisioningBackend struct {
	backend.Backend
	order []string
}

func (p *provisioningBackend) Provision(context.Context) error {
	p.order = append(p.order, "provision")
	return nil
}

func (p *provisioningBackend) Scan(ctx context.Context, m backend.ScanMode) (*publish.CurrentState, error) {
	p.order = append(p.order, "scan")
	return p.Backend.Scan(ctx, m)
}

// TestRunProvisionsBeforeScan: a backend implementing the optional Provisioner role
// has Provision called exactly once at the start of a run, before Scan. A backend
// that does not implement it (the plain fake) is simply not provisioned — the other
// pipeline tests exercise that path.
func TestRunProvisionsBeforeScan(t *testing.T) {
	b := loadBundle(t, smallBundle())
	be := &provisioningBackend{Backend: fake.New(fake.WithMaxCount(2))}

	if _, err := Run(context.Background(), be, b, WithTransportOptions(transport.WithInterval(0))); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(be.order) < 2 || be.order[0] != "provision" || be.order[1] != "scan" {
		t.Fatalf("Run should provision once before scanning, got call order %v", be.order)
	}
	provisions := 0
	for _, c := range be.order {
		if c == "provision" {
			provisions++
		}
	}
	if provisions != 1 {
		t.Errorf("Provision should be called exactly once, got %d", provisions)
	}
}
