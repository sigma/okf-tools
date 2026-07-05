package bundle

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sigma/okf-tools/internal/parser"
)

// Class is the kind of a link once resolved against the bundle. Most rules
// scope to ClassConcept — this classification is the crux of OKF correctness
// (a generic markdown linter treats all links alike; OKF does not).
type Class int

const (
	ClassConcept  Class = iota // body path-link targeting a .md inside the bundle
	ClassCitation              // link under the # Citations heading
	ClassExternal              // absolute URL (http://, mailto:, …)
	ClassWikilink              // [[obsidian]] syntax (OKF101's concern)
	ClassImage                 // ![](…) image
	ClassAnchor                // in-page #fragment or empty target
)

// ResolvedLink is a parsed link plus its on-disk resolution.
type ResolvedLink struct {
	parser.Link
	Class     Class
	Resolved  string // absolute filesystem path (concept/citation on-disk targets)
	Inside    bool   // Resolved lies within the bundle root
	Exists    bool   // Resolved names an existing file
	Absolute  bool   // target begins with "/" (bundle-absolute)
	TargetDoc *Doc   // the bundle doc this concept cross-link points at, if any
}

var schemeRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

// isURL reports whether target is an external reference rather than a path.
func isURL(target string) bool {
	if strings.HasPrefix(target, "//") {
		return true
	}
	// A scheme like http:, https:, mailto:, ftp:. A bare Windows-style drive
	// ("c:") is not a concern for OKF bundles.
	return schemeRe.MatchString(target)
}

// resolve turns a path target into an absolute path, reporting whether it was
// bundle-absolute. Fragments and queries are stripped. An empty result means an
// in-page anchor.
func (b *Bundle) resolve(fromDir, target string) (abs string, absolute bool, ok bool) {
	t := target
	if i := strings.IndexAny(t, "#?"); i >= 0 {
		t = t[:i]
	}
	if t == "" {
		return "", false, false
	}
	if strings.HasPrefix(t, "/") {
		return filepath.Join(b.Root, filepath.FromSlash(t)), true, true
	}
	return filepath.Join(fromDir, filepath.FromSlash(t)), false, true
}

func (b *Bundle) inside(abs string) bool {
	rel, err := filepath.Rel(b.Root, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func fileExists(abs string) bool {
	info, err := os.Stat(abs)
	return err == nil && !info.IsDir()
}

// classify resolves every link in d and records the results on d.Resolved,
// wiring concept cross-links to their target Doc where one exists in the bundle.
func (b *Bundle) classify(d *Doc) {
	fromDir := filepath.Dir(d.Path)
	d.Resolved = d.Resolved[:0]
	for _, l := range d.Links {
		rl := ResolvedLink{Link: l}
		switch {
		case l.Image:
			rl.Class = ClassImage
		case l.Wikilink:
			rl.Class = ClassWikilink
		case strings.HasPrefix(l.Target, "#"):
			rl.Class = ClassAnchor
		case l.InCitations:
			rl.Class = ClassCitation
			if !isURL(l.Target) {
				if abs, absolute, ok := b.resolve(fromDir, l.Target); ok {
					rl.Resolved, rl.Absolute = abs, absolute
					rl.Inside = b.inside(abs)
					rl.Exists = fileExists(abs)
				}
			}
		case isURL(l.Target):
			rl.Class = ClassExternal
		default:
			rl.Class = ClassConcept
			if abs, absolute, ok := b.resolve(fromDir, l.Target); ok {
				rl.Resolved, rl.Absolute = abs, absolute
				rl.Inside = b.inside(abs)
				rl.Exists = fileExists(abs)
				if rl.Inside {
					if rel, err := filepath.Rel(b.Root, abs); err == nil {
						rl.TargetDoc = b.byRel[filepath.ToSlash(rel)]
					}
				}
			} else {
				rl.Class = ClassAnchor
			}
		}
		d.Resolved = append(d.Resolved, rl)
	}
}
