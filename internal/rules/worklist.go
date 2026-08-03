package rules

import (
	"fmt"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/config"
)

// Category C — Worklist (OKF2xx). Advisory: defaults to info and does not fail a
// build unless a bundle escalates it via [rules]. The tool finds candidates; the
// agent decides.

func init() {
	register(&Rule{
		ID: "OKF201", Name: "orphan-pages", Category: Worklist,
		Default:   Info,
		Enabled:   func(c *config.Config) bool { return c.Worklist.Orphans != "off" },
		SevConfig: func(c *config.Config) string { return c.Worklist.Orphans },
		Check:     checkOKF201,
	})
	register(&Rule{
		ID: "OKF202", Name: "broken-links", Category: Worklist,
		Default:   Info,
		Enabled:   func(c *config.Config) bool { return c.Links.CheckBroken != "off" },
		SevConfig: func(c *config.Config) string { return c.Links.CheckBroken },
		Check:     checkOKF202,
	})
	register(&Rule{
		ID: "OKF203", Name: "heading-anchor-resolves", Category: Worklist,
		Default:   Info,
		Enabled:   func(c *config.Config) bool { return c.Links.CheckAnchors != "off" },
		SevConfig: func(c *config.Config) string { return c.Links.CheckAnchors },
		Check:     checkOKF203,
	})
	register(&Rule{
		ID: "OKF206", Name: "citation-target-exists", Category: Worklist,
		Default: Info,
		Enabled: func(c *config.Config) bool { return c.Citations.CheckTargets },
		Check:   checkOKF206,
	})
}

// OKF201: a concept no other concept links to (index/log links excluded).
func checkOKF201(ctx *Context) []Finding {
	var fs []Finding
	for _, d := range ctx.Bundle.Concepts {
		if d.Inbound == 0 {
			fs = append(fs, Finding{Path: d.Rel, Line: 0, Message: "orphan: no other concept links to this page"})
		}
	}
	return fs
}

// OKF202: a concept cross-link whose target does not resolve on disk. Defaults
// to info — a broken link may be not-yet-written knowledge (SPEC §5.3) — but a
// bundle may escalate it via [rules] or links.check_broken.
func checkOKF202(ctx *Context) []Finding {
	var fs []Finding
	for _, d := range ctx.Bundle.Concepts {
		for _, rl := range d.Resolved {
			if rl.Class != bundle.ClassConcept || rl.Exists {
				continue
			}
			fs = append(fs, Finding{Path: d.Rel, Line: rl.Line,
				Message: "broken concept link '" + rl.Target + "' (target not found; may be not-yet-written)"})
		}
	}
	return fs
}

// OKF203: a #heading anchor — cross-file (`other.md#frag`) or same-file
// (`#frag`) — that names no heading on an ordinary (non-glossary) page. It is the
// heading-anchor complement of the two rules that leave this gap: OKF202 checks a
// cross-link's *file* but ignores its fragment, and OKFEXT-GLOSSARY-02 resolves
// fragments only into/within declared glossary files. Glossary targets and
// glossary sources are skipped here so the two rules never double-report; a
// missing target file is OKF202's to flag, so this fires only when the file
// resolves. Defaults to info (SPEC §5.3: a heading may be not-yet-written), but a
// bundle may escalate it via links.check_anchors or [rules] — sigma/ideas does,
// to replace the referential-integrity gate its retired sync/ pipeline carried.
func checkOKF203(ctx *Context) []Finding {
	var fs []Finding
	for _, d := range ctx.Bundle.Docs {
		for _, rl := range d.Resolved {
			switch rl.Class {
			case bundle.ClassConcept:
				// Cross-file `other.md#frag` into an existing non-glossary page.
				t := rl.TargetDoc
				if rl.Fragment == "" || t == nil || !rl.Exists || t.Glossary {
					continue
				}
				if !t.HasHeadingAnchor(rl.Fragment) {
					fs = append(fs, Finding{Path: d.Rel, Line: rl.Line,
						Message: undefinedHeadingMsg(t, rl.Fragment)})
				}
			case bundle.ClassAnchor:
				// Same-file `#frag`; glossary self-refs are OKFEXT-GLOSSARY-02's.
				if rl.Fragment == "" || d.Glossary {
					continue
				}
				if !d.HasHeadingAnchor(rl.Fragment) {
					fs = append(fs, Finding{Path: d.Rel, Line: rl.Line,
						Message: undefinedHeadingMsg(d, rl.Fragment)})
				}
			}
		}
	}
	return fs
}

// undefinedHeadingMsg reports a #heading anchor that names no heading on doc d,
// naming the page and — when a heading slugs close by — a "did you mean" hint.
func undefinedHeadingMsg(d *bundle.Doc, frag string) string {
	msg := fmt.Sprintf("reference to undefined heading anchor '#%s' in '%s'", frag, d.Rel)
	if near := nearestHeading(d, frag); near != "" {
		msg += fmt.Sprintf(" (did you mean '#%s'?)", near)
	}
	return msg
}

// nearestHeading returns the heading slug on d closest to frag, or "" when none
// is close enough (see nearestSlug), reusing the glossary rule's hint machinery.
func nearestHeading(d *bundle.Doc, frag string) string {
	slugs := make([]string, len(d.Headings))
	for i, h := range d.Headings {
		slugs[i] = bundle.Slug(h.Text)
	}
	return nearestSlug(frag, slugs)
}

// OKF206: a citation with an on-disk target that does not exist (typo'd source).
func checkOKF206(ctx *Context) []Finding {
	var fs []Finding
	for _, d := range ctx.Bundle.Concepts {
		for _, rl := range d.Resolved {
			if rl.Class != bundle.ClassCitation || rl.Resolved == "" || rl.Exists {
				continue
			}
			fs = append(fs, Finding{Path: d.Rel, Line: rl.Line,
				Message: "citation target '" + rl.Target + "' not found on disk"})
		}
	}
	return fs
}
