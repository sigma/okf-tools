package bundle

import (
	"path"
	"sort"
	"strings"
)

// IndexEntry is a single concept link found in an index.md body.
type IndexEntry struct {
	Rel    string // target concept rel path ("" if the link doesn't resolve)
	Title  string // link text
	Desc   string // trailing description text after the link on its line
	Line   int
	Target *Doc
}

// indexByDir maps each index.md's directory (rel, "." for root) to its Doc.
func (b *Bundle) indexByDir() map[string]*Doc {
	m := make(map[string]*Doc, len(b.Indexes))
	for _, idx := range b.Indexes {
		m[path.Dir(idx.Rel)] = idx
	}
	return m
}

// Owner returns the index that owns concept c: the nearest index.md walking up
// c's directory chain, or nil if the bundle has no covering index.
func (b *Bundle) Owner(c *Doc) *Doc {
	idxByDir := b.indexByDir()
	dir := path.Dir(c.Rel)
	for {
		if idx, ok := idxByDir[dir]; ok {
			return idx
		}
		if dir == "." || dir == "" {
			return nil
		}
		parent := path.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// Scope returns the concepts owned by idx, sorted by rel path.
func (b *Bundle) Scope(idx *Doc) []*Doc {
	idxByDir := b.indexByDir()
	var out []*Doc
	for _, c := range b.Concepts {
		if b.ownerWith(idxByDir, c) == idx {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}

func (b *Bundle) ownerWith(idxByDir map[string]*Doc, c *Doc) *Doc {
	dir := path.Dir(c.Rel)
	for {
		if idx, ok := idxByDir[dir]; ok {
			return idx
		}
		if dir == "." || dir == "" {
			return nil
		}
		parent := path.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// IndexEntries parses the concept links currently listed in idx, capturing the
// trailing description on each entry's line so OKF106 can compare it against the
// target's frontmatter.
func (b *Bundle) IndexEntries(idx *Doc) []IndexEntry {
	lines := strings.Split(idx.Content, "\n")
	var out []IndexEntry
	for _, rl := range idx.Resolved {
		if rl.Class != ClassConcept {
			continue
		}
		e := IndexEntry{Line: rl.Line, Title: rl.Text, Target: rl.TargetDoc}
		if rl.TargetDoc != nil {
			e.Rel = rl.TargetDoc.Rel
		}
		if rl.Line-1 < len(lines) {
			e.Desc = descriptionAfterLink(lines[rl.Line-1])
		}
		out = append(out, e)
	}
	return out
}

// descriptionAfterLink returns the free-text description that follows a markdown
// link on an index entry line. For `* [Title](foo.md) - A widget.` it returns
// "A widget."; a leading separator (" - ", " — ", ": ") is stripped. Empty when
// there is no trailing text or no link on the line.
func descriptionAfterLink(line string) string {
	open := strings.Index(line, "](")
	if open < 0 {
		return ""
	}
	end := strings.IndexByte(line[open:], ')')
	if end < 0 {
		return ""
	}
	rest := strings.TrimSpace(line[open+end+1:])
	rest = strings.TrimLeft(rest, "-–—:") // drop the leading separator, if any
	return strings.TrimSpace(rest)
}

// entryURL renders the link target for concept c from index idx, honouring the
// bundle's link style (absolute → "/rel"; otherwise relative to idx's dir).
func (b *Bundle) entryURL(idx, c *Doc) string {
	return b.LinkURL(idx.Rel, c)
}

// RenderIndex produces canonical index.md content for idx: concepts grouped by
// type, sorted, as `* [Title](url) - description` bullets. The root index keeps
// its okf_version frontmatter.
func (b *Bundle) RenderIndex(idx *Doc) string {
	scope := b.Scope(idx)
	groups := map[string][]*Doc{}
	var types []string
	for _, c := range scope {
		t := c.Type()
		if t == "" {
			t = "Concepts"
		}
		if _, ok := groups[t]; !ok {
			types = append(types, t)
		}
		groups[t] = append(groups[t], c)
	}
	sort.Strings(types)

	var sb strings.Builder
	if idx.IsRootIndex() && b.OKFVersion != "" {
		sb.WriteString("---\nokf_version: \"" + b.OKFVersion + "\"\n---\n\n")
	}
	for gi, t := range types {
		if gi > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("# " + t + "\n\n")
		cs := groups[t]
		sort.Slice(cs, func(i, j int) bool { return cs[i].Title() < cs[j].Title() })
		for _, c := range cs {
			line := "* [" + c.Title() + "](" + b.entryURL(idx, c) + ")"
			if b.Config != nil && b.Config.Index.DescriptionsFromFrontmatter && c.Description() != "" {
				line += " - " + c.Description()
			}
			sb.WriteString(line + "\n")
		}
	}
	return sb.String()
}

// relSlash computes a relative forward-slash path from fromDir to target, both
// bundle-relative.
func relSlash(fromDir, target string) string {
	if fromDir == "." || fromDir == "" {
		return target
	}
	fromParts := strings.Split(fromDir, "/")
	tParts := strings.Split(target, "/")
	i := 0
	for i < len(fromParts) && i < len(tParts) && fromParts[i] == tParts[i] {
		i++
	}
	var out []string
	for j := i; j < len(fromParts); j++ {
		out = append(out, "..")
	}
	out = append(out, tParts[i:]...)
	return strings.Join(out, "/")
}
