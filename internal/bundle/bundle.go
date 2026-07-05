// Package bundle discovers an OKF bundle root, parses its markdown files into a
// concept/index/log model, classifies and resolves links, and builds the
// concept link graph. Rules operate on the resulting in-memory Bundle.
package bundle

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sigma/okf-tools/internal/config"
	"github.com/sigma/okf-tools/internal/parser"
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
	Inbound  int // concept cross-links pointing at this doc (orphan analysis)
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
		if !strings.HasSuffix(e.Name(), ".md") {
			return nil
		}
		doc, perr := parser.ParseFile(p)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		d := &Doc{Document: doc, Rel: rel, Base: e.Name()}
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
		b.Docs = append(b.Docs, d)
		b.byRel[rel] = d
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(b.Docs, func(i, j int) bool { return b.Docs[i].Rel < b.Docs[j].Rel })
	for _, d := range b.Docs {
		switch d.Kind {
		case KindIndex:
			b.Indexes = append(b.Indexes, d)
		case KindLog:
			b.Logs = append(b.Logs, d)
		default:
			if !d.Reserved {
				b.Concepts = append(b.Concepts, d)
			}
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
