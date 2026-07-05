package rules

import (
	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/config"
)

// Category C — Worklist (OKF2xx). Advisory, hard-capped at info, never fails a
// build. The tool finds candidates; the agent decides.

func init() {
	register(&Rule{
		ID: "OKF201", Name: "orphan-pages", Category: Worklist,
		Default: Info, HardCapInfo: true,
		Enabled:   func(c *config.Config) bool { return c.Worklist.Orphans != "off" },
		SevConfig: func(c *config.Config) string { return c.Worklist.Orphans },
		Check:     checkOKF201,
	})
	register(&Rule{
		ID: "OKF202", Name: "broken-links", Category: Worklist,
		Default: Info, HardCapInfo: true,
		Enabled:   func(c *config.Config) bool { return c.Links.CheckBroken != "off" },
		SevConfig: func(c *config.Config) string { return c.Links.CheckBroken },
		Check:     checkOKF202,
	})
	register(&Rule{
		ID: "OKF206", Name: "citation-target-exists", Category: Worklist,
		Default: Info, HardCapInfo: true,
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

// OKF202: a concept cross-link whose target does not resolve on disk. Info,
// hard-capped — a broken link may be not-yet-written knowledge (SPEC §5.3).
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
