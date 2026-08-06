package graph

import (
	"context"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
)

func TestBannerEditURL(t *testing.T) {
	// A trailing slash on the base is trimmed; the path is appended verbatim.
	bn := &Banner{BaseURL: "https://github.com/sigma/ideas/", Ref: "main"}
	if got, want := bn.editURL("docs/adr/0015.md"), "https://github.com/sigma/ideas/edit/main/docs/adr/0015.md"; got != want {
		t.Errorf("editURL = %q, want %q", got, want)
	}
}

// TestBannerEditURLPrefix: the deep-link targets the source file's *repo*-relative
// path. A node knows only its bundle-relative path, so a bundle nested in its repo
// needs the prefix joined on — without it every banner on such a bundle 404s.
// The empty-prefix case must stay byte-for-byte identical to the unprefixed URL.
func TestBannerEditURLPrefix(t *testing.T) {
	for _, tt := range []struct {
		name   string
		prefix string
		rel    string
		want   string
	}{
		{
			name:   "bundle at repo root is unchanged",
			prefix: "",
			rel:    "adr/0001.md",
			want:   "https://github.com/acme/iac/edit/main/adr/0001.md",
		},
		{
			name:   "bundle nested in the repo",
			prefix: "docs",
			rel:    "adr/0001.md",
			want:   "https://github.com/acme/iac/edit/main/docs/adr/0001.md",
		},
		{
			name:   "bundle nested two deep",
			prefix: "sub/docs",
			rel:    "index.md",
			want:   "https://github.com/acme/iac/edit/main/sub/docs/index.md",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bn := &Banner{BaseURL: "https://github.com/acme/iac", Ref: "main", Prefix: tt.prefix}
			if got := bn.editURL(tt.rel); got != tt.want {
				t.Errorf("editURL = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBannerHashFoldingPrefix: the prefix reaches the page hash through the folded
// edit URL, so fixing a wrong prefix re-publishes the affected pages instead of
// hash-skipping them and leaving the broken links in place.
func TestBannerHashFoldingPrefix(t *testing.T) {
	base := publish.Hash("base-content-hash")
	root := &Banner{Text: "GEN", BaseURL: "https://x/y", Ref: "main"}
	nested := &Banner{Text: "GEN", BaseURL: "https://x/y", Ref: "main", Prefix: "docs"}
	if root.hash(base, "a.md") == nested.hash(base, "a.md") {
		t.Error("a prefix change must change the folded hash")
	}
}

// TestBannerHashFolding: the fold is stable for an unchanged (banner, page) pair
// but changes on any of text, ref/URL, or page — so a banner edit re-publishes the
// affected pages while a steady-state re-run still hash-skips.
func TestBannerHashFolding(t *testing.T) {
	base := publish.Hash("base-content-hash")
	bn := &Banner{Text: "GEN", BaseURL: "https://x/y", Ref: "main"}
	h := bn.hash(base, "a.md")

	if h == base {
		t.Error("folded hash should differ from the base content hash")
	}
	if bn.hash(base, "a.md") != h {
		t.Error("folded hash must be stable for an unchanged banner over an unchanged page")
	}
	if (&Banner{Text: "OTHER", BaseURL: "https://x/y", Ref: "main"}).hash(base, "a.md") == h {
		t.Error("a text change must change the folded hash")
	}
	if (&Banner{Text: "GEN", BaseURL: "https://x/y", Ref: "dev"}).hash(base, "a.md") == h {
		t.Error("a ref change (URL change) must change the folded hash")
	}
	if bn.hash(base, "b.md") == h {
		t.Error("a different page (different deep-link) must change the folded hash")
	}
}

// TestGenerateBannerBlockZero: with a banner, every published page's SetContent
// carries the banner as a Quote block-0 whose single run links to that page's
// source file, ahead of the authored body.
func TestGenerateBannerBlockZero(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml":   "",
		"index.md":   "---\nokf_version: \"0.1\"\n---\n# Home\n",
		"ideas/a.md": "# A\n\nsome body\n",
	})
	bn := &Banner{Text: "GEN", BaseURL: "https://github.com/sigma/ideas", Ref: "main"}

	g, err := Generate(context.Background(), b, nil, WithBanner(bn))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	sc := opFor(g, nodeRef("ideas/a.md"), SetContent)
	if sc == nil || sc.Doc == nil {
		t.Fatal("ideas/a.md should have a SetContent op with a doc")
	}
	if len(sc.Doc.Blocks) < 2 {
		t.Fatalf("want banner + body blocks, got %d", len(sc.Doc.Blocks))
	}
	bc, ok := sc.Doc.Blocks[0].Content.(BlockContent)
	if !ok || bc.Kind != Quote {
		t.Fatalf("block-0 = %+v, want a Quote", sc.Doc.Blocks[0].Content)
	}
	if len(bc.Inlines) != 1 || bc.Inlines[0].Text != "GEN" {
		t.Fatalf("banner inlines = %+v, want one run %q", bc.Inlines, "GEN")
	}
	if got, want := bc.Inlines[0].URL, "https://github.com/sigma/ideas/edit/main/ideas/a.md"; got != want {
		t.Errorf("banner deep-link = %q, want %q", got, want)
	}
	// The banner carries no refs/anchors, so it cannot perturb edge assembly.
	if len(sc.Doc.Blocks[0].Refs) != 0 || len(sc.Doc.Blocks[0].Anchors) != 0 {
		t.Errorf("banner block must carry no refs/anchors, got %+v", sc.Doc.Blocks[0])
	}
}

// TestGenerateBannerOnEveryPage: the banner rides every page, including an index
// page, each with its own source deep-link.
func TestGenerateBannerOnEveryPage(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml":   "",
		"index.md":   "---\nokf_version: \"0.1\"\n---\n# Home\n",
		"ideas/a.md": "# A\n\nbody\n",
	})
	bn := &Banner{Text: "GEN", BaseURL: "https://h/r", Ref: "main"}
	g, err := Generate(context.Background(), b, nil, WithBanner(bn))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for rel, wantURL := range map[string]string{
		"index.md":   "https://h/r/edit/main/index.md",
		"ideas/a.md": "https://h/r/edit/main/ideas/a.md",
	} {
		sc := opFor(g, nodeRef(rel), SetContent)
		if sc == nil || sc.Doc == nil || len(sc.Doc.Blocks) == 0 {
			t.Fatalf("%s: no SetContent doc", rel)
		}
		bc := sc.Doc.Blocks[0].Content.(BlockContent)
		if bc.Kind != Quote || bc.Inlines[0].URL != wantURL {
			t.Errorf("%s: banner = %+v, want URL %q", rel, bc, wantURL)
		}
	}
}

// TestGenerateNoBannerByDefault: without WithBanner, generation is unchanged — no
// synthetic block-0 is injected.
func TestGenerateNoBannerByDefault(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml": "",
		"index.md": "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"a.md":     "# A\n\nbody\n",
	})
	g, err := Generate(context.Background(), b, nil)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	sc := opFor(g, nodeRef("a.md"), SetContent)
	if sc == nil || len(sc.Doc.Blocks) == 0 {
		t.Fatal("a.md should have content blocks")
	}
	bc := sc.Doc.Blocks[0].Content.(BlockContent)
	// The first authored block is the "# A" heading, not a Quote banner.
	if bc.Kind == Quote {
		t.Errorf("no banner expected, but block-0 is a Quote: %+v", bc)
	}
}

// TestGenerateBannerFoldedHashGatesSkip: an existing page hash-skips only when the
// scan carries the *banner-folded* hash; the plain ContentHash no longer matches,
// which is exactly what makes a banner change re-publish and prevents a false skip.
func TestGenerateBannerFoldedHashGatesSkip(t *testing.T) {
	b := loadBundle(t, map[string]string{
		"okf.toml": "",
		"index.md": "---\nokf_version: \"0.1\"\n---\nRoot.\n",
		"a.md":     "# A\n\nbody\n",
	})
	d := docByRel(t, b, "a.md")
	bn := &Banner{Text: "GEN", BaseURL: "https://h/r", Ref: "main"}
	node := nodeRef("a.md")

	// (1) Scan seeded with the banner-folded hash (and matching property hash) →
	// hash-skip (near-noop re-run).
	folded := bn.hash(ContentHash(d), "a.md")
	csFolded := publish.NewCurrentStateWithProps(
		map[publish.SymbolicID]publish.BackendID{node: "be-a"},
		map[publish.SymbolicID]publish.Hash{node: folded},
		map[publish.SymbolicID]publish.Hash{node: PropertyHash(d)},
		nil,
	)
	g, err := Generate(context.Background(), b, csFolded, WithBanner(bn))
	if err != nil {
		t.Fatal(err)
	}
	if k := opKinds(g, node); len(k) != 0 {
		t.Errorf("banner-folded hash should hash-skip, got ops %v", k)
	}

	// (2) Scan seeded with the plain (pre-banner) ContentHash → must NOT skip: the
	// banner is folded into the expected hash, so the page re-asserts.
	csPlain := publish.NewCurrentState(
		map[publish.SymbolicID]publish.BackendID{node: "be-a"},
		map[publish.SymbolicID]publish.Hash{node: ContentHash(d)},
		nil,
	)
	g2, err := Generate(context.Background(), b, csPlain, WithBanner(bn))
	if err != nil {
		t.Fatal(err)
	}
	if k := opKinds(g2, node); len(k) != 2 || k[0] != SetProperties || k[1] != SetContent {
		t.Errorf("plain-hash page should re-assert with banner, got ops %v", k)
	}
}
