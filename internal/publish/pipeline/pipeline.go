package pipeline

import (
	"context"
	"fmt"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/optimize"
	"github.com/sigma/okf-tools/internal/publish/transport"
)

// Result summarizes one publish for the caller: the resolution-table updates the
// run produced (created nodes and hosted anchors) and how many transactions the
// backend actually executed. TxnCount is the near-noop witness: an unchanged
// re-run drives it to (near) zero because Generation hash-skips every unchanged
// page, so Optimization emits an empty transaction-DAG and Transport calls the
// backend zero times.
type Result struct {
	// Nodes maps each symbolic id created this run to its minted backend id.
	Nodes map[publish.SymbolicID]publish.BackendID
	// Anchors maps each anchor hosted this run to its minted backend id.
	Anchors map[publish.AnchorName]publish.BackendID
	// TxnCount is the number of transactions Transport executed against the backend
	// this run — the backend-call count a scheduled publish wants near zero.
	TxnCount int
}

// Option configures Run.
type Option func(*options)

type options struct {
	transportOpts []transport.Option
	scanMode      backend.ScanMode
}

// WithTransportOptions forwards options to Stage 3 (Transport) — chiefly the
// per-Group pacing interval (transport.WithInterval), which tests set to 0 to run
// without wall-clock delay.
func WithTransportOptions(opts ...transport.Option) Option {
	return func(o *options) { o.transportOpts = append(o.transportOpts, opts...) }
}

// WithScanMode selects the scan producer mode: backend.ScanStored (the cheap
// steady-state default) or backend.ScanRecompute (the opt-in full live-block walk
// for true drift detection and subpage/anchor self-healing). Steady-state publishes
// leave it unset; a scheduled/periodic drift sweep opts into recompute.
func WithScanMode(mode backend.ScanMode) Option {
	return func(o *options) { o.scanMode = mode }
}

// Run drives one publish of b against be, wiring the three stages behind a single
// call: it scans current state once, generates the backend-neutral op-DAG against
// that scan, optimizes it into a transaction-DAG, and drains it through the
// transport (seeding the resolution table from the same scan). The scan is taken
// exactly once and threaded into both Generation (change detection) and Transport
// (resolution seed), which is what makes an unchanged re-run a near-noop: every
// page hash-skips, the DAG is empty, and no backend write happens.
//
// Run performs backend I/O only through be — Scan and Execute — so it is offline
// whenever be is (the fake, or a Notion backend aimed at an httptest server).
func Run(ctx context.Context, be backend.Backend, b *bundle.Bundle, opts ...Option) (*Result, error) {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}

	scan, err := be.Scan(ctx, o.scanMode)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	g, err := graph.Generate(ctx, b, scan)
	if err != nil {
		return nil, fmt.Errorf("generate: %w", err)
	}

	dag := optimize.Optimize(g, be, be)

	res, err := transport.New(be, o.transportOpts...).Run(ctx, dag, scan)
	if err != nil {
		return nil, fmt.Errorf("transport: %w", err)
	}

	return &Result{
		Nodes:    res.Nodes,
		Anchors:  res.Anchors,
		TxnCount: len(dag.Txns),
	}, nil
}
