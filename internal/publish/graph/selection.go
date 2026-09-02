package graph

import (
	"path"
	"strings"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
)

// selectionScope decides what a run publishes and what happens to a reference
// pointing out of it.
//
// Narrowing a run is not only a filter on which pages get ops: a cross-reference
// leaving the selection must stop being a late-bound Ref, because the transport
// gates each transaction on its Refs resolving and a node this run never creates
// never will. Left alone it deadlocks the drain — which is exactly how the gap was
// found (sigma/okf-tools#161).
type selectionScope struct {
	// contains reports whether a bundle-relative page is in this run.
	contains func(rel string) bool
	// banner supplies the repo web coordinates a demoted link points at. Nil when
	// no banner is configured, in which case a demoted link degrades to plain text
	// rather than inventing a URL.
	banner *Banner
}

// newSelectionScope returns nil when no selection is set, so the un-narrowed path
// (every existing backend) allocates nothing and behaves identically.
func newSelectionScope(contains func(rel string) bool, banner *Banner) *selectionScope {
	if contains == nil {
		return nil
	}
	return &selectionScope{contains: contains, banner: banner}
}

// includes reports whether a page belongs to this run. A nil scope includes
// everything.
func (s *selectionScope) includes(rel string) bool {
	if s == nil {
		return true
	}
	return s.contains(rel)
}

// demote reports how an out-of-selection reference should render instead: its
// visible text and, where the repo's web address is known, a link to the source
// file. It reports false for a reference that stays a Ref.
//
// Anchor references are never demoted: the glossary host is appended to every
// selection, so an anchor's host is in scope by construction.
func (s *selectionScope) demote(id publish.SymbolicID, rl *bundle.ResolvedLink) (text, url string, demoted bool) {
	if s == nil {
		return "", "", false
	}
	if _, isAnchor := id.AnchorName(); isAnchor {
		return "", "", false
	}
	rel := id.Rel()
	if s.contains(rel) {
		return "", "", false
	}

	text = ""
	if rl != nil {
		text = rl.Text
	}
	if text == "" {
		text = strings.TrimSuffix(path.Base(rel), ".md")
	}
	if s.banner != nil && s.banner.BaseURL != "" {
		url = s.banner.editURL(rel)
	}
	return text, url, true
}

// withAreaRoots splices each area's root README.md into the publish set, placed
// immediately BEFORE the first page of its own directory so a document opens with
// its area's overview.
//
// Position is not left to the path sort: "concepts/README.md" happens to sort
// before "concepts/alpha.md" only because uppercase precedes lowercase, and it
// would sort after "concepts/ADR-index.md". Relying on that accident means the
// overview silently stops being first the day someone adds such a page
// (sigma/okf-tools#163).
func withAreaRoots(docs []*bundle.Doc, roots []*bundle.Doc) []*bundle.Doc {
	if len(roots) == 0 {
		return docs
	}
	byDir := make(map[string]*bundle.Doc, len(roots))
	for _, r := range roots {
		byDir[path.Dir(r.Rel)] = r
	}

	out := make([]*bundle.Doc, 0, len(docs)+len(roots))
	placed := make(map[string]bool, len(roots))
	for _, d := range docs {
		dir := path.Dir(d.Rel)
		if root, ok := byDir[dir]; ok && !placed[dir] {
			out = append(out, root)
			placed[dir] = true
		}
		out = append(out, d)
	}
	// An area whose only page IS its README has no page to precede, so append it.
	for dir, root := range byDir {
		if !placed[dir] {
			out = append(out, root)
		}
	}
	return out
}
