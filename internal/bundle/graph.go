package bundle

import "sort"

// buildGraph counts, for every concept, how many other concepts link to it via
// a concept cross-link. Links from index/log files and self-links do not count
// (per RULES.md orphan analysis). Called once after classification.
func (b *Bundle) buildGraph() {
	for _, d := range b.Concepts {
		for _, rl := range d.Resolved {
			if rl.Class != ClassConcept || rl.TargetDoc == nil {
				continue
			}
			if rl.TargetDoc == d {
				continue // self-link
			}
			rl.TargetDoc.Inbound++
		}
	}
}

// Edge is a directed concept→concept link in the graph.
type Edge struct {
	From string `json:"from"` // source concept rel path
	To   string `json:"to"`   // target concept rel path
}

// Graph returns the concept nodes (rel paths, sorted) and the deduplicated set
// of concept→concept edges, for `okf graph`.
func (b *Bundle) Graph() (nodes []*Doc, edges []Edge) {
	nodes = append(nodes, b.Concepts...)
	seen := map[Edge]bool{}
	for _, d := range b.Concepts {
		for _, rl := range d.Resolved {
			if rl.Class != ClassConcept || rl.TargetDoc == nil || rl.TargetDoc == d {
				continue
			}
			e := Edge{From: d.Rel, To: rl.TargetDoc.Rel}
			if !seen[e] {
				seen[e] = true
				edges = append(edges, e)
			}
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	return nodes, edges
}

// Title returns a concept's display title: the frontmatter title, else the
// document's top-level (level-1) H1, else its rel path. The H1 fallback keeps
// frontmatter-less pages (research notes, etc.) from publishing under their file
// path. See #99.
func (d *Doc) Title() string {
	if d.Frontmatter != nil {
		if t, ok := d.Frontmatter["title"].(string); ok && t != "" {
			return t
		}
	}
	if h := d.firstH1(); h != "" {
		return h
	}
	return d.Rel
}

// firstH1 returns the text of the document's first top-level (level-1) heading,
// or "" when it has none.
func (d *Doc) firstH1() string {
	for _, h := range d.Headings {
		if h.Level == 1 {
			return h.Text
		}
	}
	return ""
}

// Type returns a concept's type: the frontmatter type, else the declared type of
// the area that owns the page (AreaType, resolved from areas.json at load), else
// "". The area fallback types unauthored pages by their directory — the area
// *is* the type when the page declares none. See #99.
func (d *Doc) Type() string {
	if d.Frontmatter != nil {
		if t, ok := d.Frontmatter["type"].(string); ok && t != "" {
			return t
		}
	}
	return d.AreaType
}

// Description returns a concept's frontmatter description, or "".
func (d *Doc) Description() string {
	if d.Frontmatter != nil {
		if s, ok := d.Frontmatter["description"].(string); ok {
			return s
		}
	}
	return ""
}
