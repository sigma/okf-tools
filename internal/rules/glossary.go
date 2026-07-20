package rules

import (
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
