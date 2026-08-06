package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/source"
)

// nestedBundleFixture is the one fixture whose bundle root is not its repo root:
// the bundle lives at docs/, so a node's bundle-relative path (adr/0001-...md)
// differs from its repo-relative path (docs/adr/0001-...md). Every other fixture
// has the two coincide, which is exactly what let the banner deep-link conflate
// them.
func nestedBundleFixture(t *testing.T) *bundle.Bundle {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "testdata", "nested-bundle", "docs")
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

// TestNestedBundleBannerDeepLinks walks the whole path the bug lived on —
// environment → source resolution → banner → generated block-0 URL — against a
// bundle that really is nested inside its repo on disk.
//
// It drives resolution through OKF_SOURCE_PREFIX rather than a real checkout: the
// git tier is covered by the resolver's own injected-git tests, and requiring a
// `git init` here would buy nothing but setup.
func TestNestedBundleBannerDeepLinks(t *testing.T) {
	b := nestedBundleFixture(t)

	src, err := source.Resolve(func(k string) string {
		switch k {
		case source.EnvSourceURL:
			return "https://github.com/acme/iac"
		case source.EnvSourceRef:
			return "main"
		case source.EnvSourcePrefix:
			return "docs"
		}
		return ""
	}, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	bn := &graph.Banner{Text: "GEN", BaseURL: src.BaseURL, Ref: src.Ref, Prefix: src.Prefix}
	g, err := graph.Generate(context.Background(), b, nil, graph.WithBanner(bn))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Every page in the fixture, including the one in the adr/ subtree, must link to
	// its docs/-prefixed source path.
	want := map[publish.SymbolicID]string{
		publish.NodeRef("index.md"):                   "https://github.com/acme/iac/edit/main/docs/index.md",
		publish.NodeRef("publishing.md"):              "https://github.com/acme/iac/edit/main/docs/publishing.md",
		publish.NodeRef("adr/index.md"):               "https://github.com/acme/iac/edit/main/docs/adr/index.md",
		publish.NodeRef("adr/0001-nested-bundles.md"): "https://github.com/acme/iac/edit/main/docs/adr/0001-nested-bundles.md",
	}
	got := bannerURLs(t, g)
	for node, wantURL := range want {
		if got[node] != wantURL {
			t.Errorf("%s: banner deep-link = %q, want %q", node, got[node], wantURL)
		}
	}
	if len(got) != len(want) {
		t.Errorf("banner pages = %d (%v), want %d", len(got), got, len(want))
	}
}

// TestNestedBundleWithoutPrefixOmitsDir is the regression witness for #124: with no
// prefix resolved, the emitted links drop the docs/ segment and point at paths that
// do not exist in the repo. Pinning the broken shape keeps the fix from silently
// regressing to it.
func TestNestedBundleWithoutPrefixOmitsDir(t *testing.T) {
	b := nestedBundleFixture(t)
	bn := &graph.Banner{Text: "GEN", BaseURL: "https://github.com/acme/iac", Ref: "main"}
	g, err := graph.Generate(context.Background(), b, nil, graph.WithBanner(bn))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got, want := bannerURLs(t, g)[publish.NodeRef("adr/index.md")], "https://github.com/acme/iac/edit/main/adr/index.md"; got != want {
		t.Errorf("unprefixed deep-link = %q, want %q", got, want)
	}
}

// bannerURLs collects each generated page's block-0 banner deep-link, keyed by the
// page's symbolic id.
func bannerURLs(t *testing.T, g *graph.Graph) map[publish.SymbolicID]string {
	t.Helper()
	urls := map[publish.SymbolicID]string{}
	for _, op := range g.Ops {
		if op.Kind != graph.SetContent || op.Doc == nil || len(op.Doc.Blocks) == 0 {
			continue
		}
		bc, ok := op.Doc.Blocks[0].Content.(graph.BlockContent)
		if !ok || bc.Kind != graph.Quote || len(bc.Inlines) == 0 {
			continue
		}
		urls[op.Node] = bc.Inlines[0].URL
	}
	return urls
}
