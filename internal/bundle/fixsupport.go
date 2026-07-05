package bundle

import (
	"path"
	"strings"
)

// RelSlash exposes the relative forward-slash path computation for callers that
// rewrite links (e.g. the autofixer).
func RelSlash(fromDir, target string) string { return relSlash(fromDir, target) }

// LinkURL renders a link from the file at fromRel to concept to, honouring the
// bundle's configured link style (absolute → "/rel"; otherwise relative).
func (b *Bundle) LinkURL(fromRel string, to *Doc) string {
	if b.Config != nil && b.Config.Links.Style == "absolute" {
		return "/" + to.Rel
	}
	return relSlash(path.Dir(fromRel), to.Rel)
}

// ResolveWikilink returns the single concept a wikilink target names — by rel
// path or basename, case-insensitively, ignoring any #fragment — or nil when
// zero or more than one concept matches (i.e. the rewrite is ambiguous).
func (b *Bundle) ResolveWikilink(target string) *Doc {
	t := target
	if i := strings.IndexByte(t, '#'); i >= 0 {
		t = t[:i]
	}
	t = strings.TrimSpace(t)
	if t == "" {
		return nil
	}
	norm := strings.ToLower(strings.TrimSuffix(t, ".md"))
	var matches []*Doc
	for _, c := range b.Concepts {
		if strings.ToLower(strings.TrimSuffix(c.Rel, ".md")) == norm ||
			strings.ToLower(strings.TrimSuffix(c.Base, ".md")) == norm {
			matches = append(matches, c)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return nil
}
