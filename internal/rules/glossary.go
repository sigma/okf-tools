package rules

import (
	"fmt"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/config"
)

// Glossary extension (OKFEXT-GLOSSARY-*). A built-in, non-spec extension gated on
// glossary.enabled and scoped to the declared glossary files (config [glossary]
// files). It treats a single Markdown file as an anchor-addressable glossary —
// "a glossary is to terms what index.md is to pages" — implementing the
// domain-modeling CONTEXT-FORMAT, not the OKF spec. Defaults to warning so a
// bundle can promote any of these to a hard failure via [rules].

func init() {
	register(&Rule{
		ID: "OKFEXT-GLOSSARY-01", Name: "glossary-structure", Category: Extension,
		Default: Warning,
		Enabled: func(c *config.Config) bool { return c.Glossary.Enabled },
		Check:   checkGlossaryStructure,
	})
	register(&Rule{
		ID: "OKFEXT-GLOSSARY-02", Name: "glossary-anchor-resolves", Category: Extension,
		Default: Warning,
		Enabled: func(c *config.Config) bool { return c.Glossary.Enabled },
		Check:   checkGlossaryAnchor,
	})
	register(&Rule{
		ID: "OKFEXT-GLOSSARY-03", Name: "glossary-term-unique", Category: Extension,
		Default: Warning,
		Enabled: func(c *config.Config) bool { return c.Glossary.Enabled },
		Check:   checkGlossaryTermUnique,
	})
	register(&Rule{
		ID: "OKFEXT-GLOSSARY-04", Name: "glossary-orphan-term", Category: Extension,
		Default: Info,
		Enabled: func(c *config.Config) bool { return c.Glossary.Enabled },
		Check:   checkGlossaryOrphanTerm,
	})
}

// OKFEXT-GLOSSARY-01: a declared glossary file is term-structured per
// CONTEXT-FORMAT — the glossary analogue of OKF003/OKF004. Its body is bold-lead
// term entries and optional grouping headings. Prose intros are tolerated, but
// every list item must parse as a well-formed `**Term**: definition`, and a
// glossary that extracts zero terms is flagged.
func checkGlossaryStructure(ctx *Context) []Finding {
	var fs []Finding
	for _, d := range ctx.Bundle.Glossaries {
		if len(d.Terms) == 0 {
			fs = append(fs, Finding{Path: d.Rel, Line: d.BodyStartLine,
				Message: "declared glossary defines no terms; expected CONTEXT-FORMAT '**Term**: definition' entries"})
		}
		termLines := make(map[int]bool, len(d.Terms))
		for _, t := range d.Terms {
			termLines[t.Line] = true
		}
		for _, li := range d.ListItems {
			if !termLines[li.Line] {
				fs = append(fs, Finding{Path: d.Rel, Line: li.Line,
					Message: "glossary entry is not a well-formed term; expected '**Term**: definition'"})
			}
		}
	}
	return fs
}

// OKFEXT-GLOSSARY-02 (the load-bearing rule): a concept link into a declared
// glossary file that carries a #fragment must resolve to a defined anchor (term
// slug or heading slug) in that file — "a reference to an undefined concept,"
// caught at lint time. In-page #fragments are checked too, but only when the
// source is itself a glossary file (a self-reference); general bundle-wide
// heading-anchor resolution stays out of scope.
func checkGlossaryAnchor(ctx *Context) []Finding {
	var fs []Finding
	for _, d := range ctx.Bundle.Docs {
		for _, rl := range d.Resolved {
			switch rl.Class {
			case bundle.ClassConcept:
				if rl.TargetDoc != nil && rl.TargetDoc.Glossary && rl.Fragment != "" && !rl.TargetDoc.HasAnchor(rl.Fragment) {
					fs = append(fs, Finding{Path: d.Rel, Line: rl.Line,
						Message: undefinedAnchorMsg(rl.TargetDoc, rl.Fragment)})
				}
			case bundle.ClassAnchor:
				if d.Glossary && rl.Fragment != "" && !d.HasAnchor(rl.Fragment) {
					fs = append(fs, Finding{Path: d.Rel, Line: rl.Line,
						Message: undefinedAnchorMsg(d, rl.Fragment)})
				}
			}
		}
	}
	return fs
}

// OKFEXT-GLOSSARY-03: within a glossary file, term slugs are unique and do not
// collide with heading slugs — otherwise a #anchor is ambiguous and unstable
// across renders (GitHub/Notion). Flags any slug produced by more than one term,
// or by a term and a heading, at the later occurrence's line.
func checkGlossaryTermUnique(ctx *Context) []Finding {
	var fs []Finding
	for _, g := range ctx.Bundle.Glossaries {
		first := map[string]bundle.Anchor{}
		for _, a := range g.Anchors { // sorted by line
			prev, seen := first[a.Slug]
			if !seen {
				first[a.Slug] = a
				continue
			}
			// A collision matters only when at least one side is a term.
			if a.Kind == bundle.AnchorTerm || prev.Kind == bundle.AnchorTerm {
				fs = append(fs, Finding{Path: g.Rel, Line: a.Line,
					Message: fmt.Sprintf("anchor slug '%s' (%s '%s') collides with %s '%s' at line %d; glossary anchors must be unique",
						a.Slug, a.Kind, a.Text, prev.Kind, prev.Text, prev.Line)})
			}
		}
	}
	return fs
}

// OKFEXT-GLOSSARY-04 (stretch, Worklist/info): a defined glossary term that no
// concept references by anchor — the term-granularity analogue of OKF201
// orphan-pages. Advisory: a freshly-authored term may simply not be linked yet.
func checkGlossaryOrphanTerm(ctx *Context) []Finding {
	var fs []Finding
	for _, g := range ctx.Bundle.Glossaries {
		// Slugs referenced by any concept cross-link into this glossary file.
		referenced := map[string]bool{}
		for _, d := range ctx.Bundle.Docs {
			for _, rl := range d.Resolved {
				if rl.Class == bundle.ClassConcept && rl.TargetDoc == g && rl.Fragment != "" {
					referenced[rl.Fragment] = true
				}
			}
		}
		for _, a := range g.Anchors {
			if a.Kind != bundle.AnchorTerm || referenced[a.Slug] {
				continue
			}
			fs = append(fs, Finding{Path: g.Rel, Line: a.Line,
				Message: fmt.Sprintf("orphan term: '%s' (#%s) is defined but no concept links to it", a.Text, a.Slug)})
		}
	}
	return fs
}

// undefinedAnchorMsg reports a missing glossary anchor, naming the file and — if
// a close match exists — the nearest defined anchor as a "did you mean" hint.
func undefinedAnchorMsg(g *bundle.Doc, frag string) string {
	msg := fmt.Sprintf("reference to undefined glossary anchor '#%s' in '%s'", frag, g.Rel)
	if near := nearestAnchor(g, frag); near != "" {
		msg += fmt.Sprintf(" (did you mean '#%s'?)", near)
	}
	return msg
}

// nearestAnchor returns the defined anchor slug closest to frag by edit distance,
// or "" when nothing is within a small threshold (so we don't invent noise).
func nearestAnchor(g *bundle.Doc, frag string) string {
	best, bestDist := "", 1<<30
	for _, a := range g.Anchors {
		if d := levenshtein(frag, a.Slug); d < bestDist {
			best, bestDist = a.Slug, d
		}
	}
	// Only suggest a genuinely close match: within a third of the length, min 2.
	limit := len(frag) / 3
	if limit < 2 {
		limit = 2
	}
	if bestDist <= limit {
		return best
	}
	return ""
}

// levenshtein is the classic edit distance between two short strings.
func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
