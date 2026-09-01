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
	// Stats is what the run cost in API traffic, for a backend that meters it (the
	// backend.RequestReporter role). It stays the zero value for a backend that does
	// not — the fake and the filesystem export issue no requests.
	Stats publish.RequestStats
	// Metered says whether Stats was actually reported, so a caller can tell "this
	// backend issued no requests" from "this backend counts none". Without it, zeros
	// are ambiguous and a bin has to re-derive the answer from the backend's concrete
	// type — the switch the RequestReporter role exists to avoid.
	Metered bool
}

// Option configures Run.
type Option func(*options)

type options struct {
	scanMode backend.ScanMode
	banner   *graph.Banner
}

// WithBanner threads a resolved generated-page disclaimer banner into Generation,
// so every published page carries it as block-0 and a banner/URL change re-syncs
// the affected pages (sigma/ideas ADR-0015). The banner's source-repo coordinates
// are resolved by the bin (cmd/okfpub, via internal/publish/source) and passed in
// as data; Run itself reads no environment. Unset (the default) publishes no
// banner, so callers that don't opt in — including every existing test — are
// unaffected.
func WithBanner(bn *graph.Banner) Option {
	return func(o *options) { o.banner = bn }
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

	if p, ok := be.(backend.Provisioner); ok {
		if err := p.Provision(ctx); err != nil {
			return nil, fmt.Errorf("provision: %w", err)
		}
	}

	scan, err := be.Scan(ctx, o.scanMode)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	var genOpts []graph.Option
	if o.banner != nil {
		genOpts = append(genOpts, graph.WithBanner(o.banner))
	}
	// A backend that reconstructs a matching content hash from its live scan supplies
	// a source-side hasher so change detection compares like against like. Without it
	// the Notion --recompute path can never hash-skip and re-clobbers every node every
	// run (#110). The hasher owns banner handling (it hashes the realized block-0), so
	// it is threaded whether or not a banner is set, and for both scan modes so the
	// stored hash stays consistent across them.
	if ch, ok := be.(graph.Recomputer); ok {
		genOpts = append(genOpts, graph.WithHasher(ch.RecomputeContentHasher(o.banner)))
	}
	g, err := graph.Generate(ctx, b, scan, genOpts...)
	if err != nil {
		return nil, fmt.Errorf("generate: %w", err)
	}

	dag := optimize.Optimize(g, be, be)

	res, err := transport.New(be).Run(ctx, dag, scan)
	if err != nil {
		return nil, fmt.Errorf("transport: %w", err)
	}

	out := &Result{
		Nodes:    res.Nodes,
		Anchors:  res.Anchors,
		TxnCount: len(dag.Txns),
	}
	// Read the traffic meter last, so it covers every stage that issued requests:
	// Provision, Scan, the drain, and write-back alike.
	if rr, ok := be.(backend.RequestReporter); ok {
		out.Stats, out.Metered = rr.RequestStats(), true
	}
	return out, nil
}
