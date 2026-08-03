// Package bundle discovers an OKF bundle root, parses its markdown files into a
// concept/index/log model, classifies and resolves links, and builds the
// concept link graph. Rules operate on the resulting in-memory Bundle.
package bundle

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sigma/okf-tools/internal/areas"
	"github.com/sigma/okf-tools/internal/config"
	"github.com/sigma/okf-tools/internal/parser"
	"github.com/sigma/okf-tools/internal/schema"
)

// DocKind distinguishes concept pages from the two reserved structural files.
type DocKind int

const (
	KindConcept DocKind = iota
	KindIndex
	KindLog
)

// Doc is a single markdown file within the bundle.
type Doc struct {
	*parser.Document
	Rel      string // bundle-relative path, forward slashes
	Base     string // filepath.Base(Rel)
	Kind     DocKind
	Reserved bool
	Resolved []ResolvedLink
	Inbound  int      // concept cross-links pointing at this doc (orphan analysis)
	Glossary bool     // declared as a glossary file (config [glossary] files)
	Anchors  []Anchor // anchor-addressable targets, when Glossary (term + heading slugs)
	// AreaType is the declared type of the areas.json area that owns this page,
	// resolved once at load time; "" when no area claims it or there is no
	// registry. Type() falls back to it when the page carries no frontmatter
	// type, so an unauthored page still types by its area (the area *is* the
	// type). See #99.
	AreaType string
}

// Bundle is the in-memory model rules run against.
type Bundle struct {
	Root       string // absolute bundle root
	Config     *config.Config
	OKFVersion string
	Docs       []*Doc
	Concepts   []*Doc
	Indexes    []*Doc
	Logs       []*Doc
	Glossaries []*Doc // declared glossary files (config [glossary] files)

	// Schema is the parsed /schema.json when the frontmatter-schema extension is
	// enabled (config [schema]); nil otherwise. OKFEXT-SCHEMA-01 lints against it.
	Schema *schema.Schema

	// Areas is the parsed repo-root /areas.json registry when present; nil when a
	// bundle is configured entirely through okf.toml. It designates the
	// glossary/anchor-host area via a role marker (areas.RoleGlossary), the config
	// contract okftool and okfpub share instead of a hardwired anchor-host path.
	Areas *areas.Registry

	byRel map[string]*Doc
}

// Discover locates the bundle root and config path. bundleFlag/configFlag come
// from --bundle/--config; startDir seeds the upward search when bundleFlag is
// empty. Resolution order: explicit --bundle → nearest okf.toml → nearest
// index.md declaring okf_version.
func Discover(startDir, bundleFlag, configFlag string) (root, configPath string, err error) {
	if bundleFlag != "" {
		root, err = filepath.Abs(bundleFlag)
		if err != nil {
			return "", "", err
		}
		configPath = pickConfig(root, configFlag)
		return root, configPath, nil
	}

	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", "", err
	}
	for {
		if p := filepath.Join(dir, "okf.toml"); fileExists(p) {
			return dir, pickConfig(dir, configFlag), nil
		}
		if p := filepath.Join(dir, "index.md"); fileExists(p) && declaresOKFVersion(p) {
			return dir, pickConfig(dir, configFlag), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", fmt.Errorf("no okf bundle found from %s (need okf.toml or an index.md with okf_version)", startDir)
		}
		dir = parent
	}
}

func pickConfig(root, configFlag string) string {
	if configFlag != "" {
		return configFlag
	}
	if p := filepath.Join(root, "okf.toml"); fileExists(p) {
		return p
	}
	return ""
}

func declaresOKFVersion(indexPath string) bool {
	d, err := parser.ParseFile(indexPath)
	if err != nil {
		return false
	}
	_, ok := d.Frontmatter["okf_version"]
	return ok
}

// Load builds the Bundle rooted at the discovered root using the config at
// configPath (empty for defaults). The config's [bundle].root is honoured
// relative to the config file's directory.
func Load(root, configPath string) (*Bundle, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if configPath != "" && cfg.Bundle.Root != "" && cfg.Bundle.Root != "." {
		root = filepath.Join(filepath.Dir(configPath), cfg.Bundle.Root)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	b := &Bundle{Root: root, Config: cfg, byRel: map[string]*Doc{}}
	reserved := cfg.ReservedSet()

	// The frontmatter-schema extension (OKFEXT-SCHEMA-01) loads its authoritative
	// /schema.json up front — relative to the bundle root — so a malformed or
	// missing schema fails the whole load loudly rather than silently linting
	// nothing. Parsed once here; the rule reads b.Schema.
	if cfg.Schema.Enabled {
		sc, serr := schema.Load(filepath.Join(root, cfg.Schema.File))
		if serr != nil {
			return nil, fmt.Errorf("load schema: %w", serr)
		}
		b.Schema = sc
	}

	// The repo-root /areas.json is the config contract that designates the
	// glossary/anchor host via a role marker (areas.RoleGlossary). It is optional
	// — a bundle configured entirely through okf.toml has none — but authoritative
	// when present, so a malformed or ambiguous registry fails the load loudly
	// rather than silently mis-resolving the anchor host. glossaryRel is the
	// marker-designated anchor-host file, resolved from the role, never from a
	// filename literal.
	var glossaryRel string
	if p := filepath.Join(root, "areas.json"); fileExists(p) {
		reg, aerr := areas.Load(p)
		if aerr != nil {
			return nil, fmt.Errorf("load areas: %w", aerr)
		}
		b.Areas = reg
		if f, ok := reg.GlossaryFile(); ok {
			glossaryRel = normScope(f)
		}
	}

	err = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if p != root && strings.HasPrefix(e.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		// Skip symlinked entries. A cluster keeps a README.md symlink to its
		// canonical index.md so GitHub renders the folder entry, but parser
		// resolves the link, so publishing the symlink would emit a second copy
		// of the target document. The real target is walked and published
		// normally; cross-links resolve against it. See #91.
		if e.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			return nil
		}
		doc, perr := parser.ParseFile(p)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		d := &Doc{Document: doc, Rel: rel, Base: e.Name(), AreaType: b.Areas.TypeFor(rel)}
		switch {
		case reserved[e.Name()] && e.Name() == "index.md":
			d.Kind, d.Reserved = KindIndex, true
		case reserved[e.Name()] && e.Name() == "log.md":
			d.Kind, d.Reserved = KindLog, true
		case reserved[e.Name()]:
			d.Reserved = true
		default:
			d.Kind = KindConcept
		}
		// A declared glossary file is a third structured page kind: exempt from
		// the concept conformance rules (it carries no frontmatter by design),
		// like index.md/log.md. It is designated by okf.toml's [glossary] files or
		// by the areas.json role marker (glossaryRel); the two are unioned so the
		// marker adds an anchor host without disturbing a bundle that names its
		// glossary the old way. Both paths honour [glossary].enabled as the master
		// opt-in, so a bundle that hasn't enabled the extension is unaffected.
		if cfg.IsGlossary(rel) || (cfg.Glossary.Enabled && glossaryRel != "" && rel == glossaryRel) {
			d.Glossary = true
		}
		b.Docs = append(b.Docs, d)
		b.byRel[rel] = d
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(b.Docs, func(i, j int) bool { return b.Docs[i].Rel < b.Docs[j].Rel })
	for _, d := range b.Docs {
		switch {
		case d.Glossary:
			b.Glossaries = append(b.Glossaries, d)
		case d.Kind == KindIndex:
			b.Indexes = append(b.Indexes, d)
		case d.Kind == KindLog:
			b.Logs = append(b.Logs, d)
		case !d.Reserved:
			b.Concepts = append(b.Concepts, d)
		}
	}

	if root := b.byRel["index.md"]; root != nil {
		if v, ok := root.Frontmatter["okf_version"]; ok {
			b.OKFVersion = fmt.Sprint(v)
		}
	}

	citationPred := citationHeadingPred(cfg.Citations.Heading)
	for _, d := range b.Docs {
		d.MarkCitations(citationPred)
		b.classify(d)
		if d.Glossary {
			buildAnchors(d)
		}
	}
	b.buildGraph()
	return b, nil
}

// citationHeadingPred matches the configured citations heading by its text,
// case-insensitively, ignoring the leading #s.
func citationHeadingPred(heading string) func(parser.Heading) bool {
	want := strings.ToLower(strings.TrimSpace(strings.TrimLeft(heading, "# ")))
	return func(h parser.Heading) bool {
		return strings.ToLower(h.Text) == want
	}
}

// Rel returns the bundle-relative, forward-slash path for an absolute path.
func (b *Bundle) Rel(abs string) string {
	rel, err := filepath.Rel(b.Root, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

// IsRootIndex reports whether d is the bundle-root index.md (the one file that
// may carry an okf_version frontmatter key).
func (d *Doc) IsRootIndex() bool { return d.Kind == KindIndex && d.Rel == "index.md" }

// InPublishScope reports whether a bundle-relative page path lies within the
// export scope declared by areas.json — under a declared area directory, or the
// declared glossary/anchor-host file (the single file-backed area, e.g.
// CONTEXT.md). When the bundle carries no areas.json registry (an okf.toml-only
// bundle), every page is in scope: there is no declared area set to narrow to, so
// behaviour is unchanged.
//
// This narrows only what is *published*, never what is loaded: Load still ingests
// the whole tree and resolves cross-links there, and PublishDocs applies this
// predicate to gate the node set the publish pipeline emits ops for. It is the
// publish-side analogue of the lint command's path-list narrowing.
func (b *Bundle) InPublishScope(rel string) bool {
	if b.Areas == nil {
		return true
	}
	rel = normScope(rel)
	// The glossary/anchor host is published even though it is a single file, not a
	// directory area. It is resolved from the areas.json role marker, never a
	// filename literal.
	if host, ok := b.Areas.GlossaryFile(); ok && rel == normScope(host) {
		return true
	}
	// An area's own root README.md is that area's section-landing page. An
	// areas.json area maps to the unified *database*, so its landing README is not
	// a row in that database — the production mirror omits it, and publishing it
	// inflates the top-level row set. Skip it. (A README inside a *sub*directory of
	// an area is a cluster's entry point, not an area root: it stays in scope and
	// becomes the nesting parent for its siblings — see the publish-graph
	// hierarchy and areas.Registry.IsAreaRoot.)
	if path.Base(rel) == "README.md" && b.Areas.IsAreaRoot(path.Dir(rel)) {
		return false
	}
	// Otherwise a page is in scope iff it lives under a declared area directory.
	for _, a := range b.Areas.Areas {
		if a.Directory == "" {
			continue
		}
		d := normScope(a.Directory)
		if d == "." || rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

// PublishDocs returns the subset of loaded Docs within the export scope (see
// InPublishScope), preserving Docs' order. The full Docs slice stays intact for
// link resolution; only the publish pipeline consumes this narrowed view.
func (b *Bundle) PublishDocs() []*Doc {
	if b.Areas == nil {
		return b.Docs
	}
	out := make([]*Doc, 0, len(b.Docs))
	for _, d := range b.Docs {
		if b.InPublishScope(d.Rel) {
			out = append(out, d)
		}
	}
	return out
}

// normScope normalizes a scope path (an area's directory/file, or a Doc.Rel) to
// the same shape: forward slashes, no leading slash, cleaned.
func normScope(p string) string {
	return path.Clean(strings.TrimPrefix(filepath.ToSlash(p), "/"))
}
