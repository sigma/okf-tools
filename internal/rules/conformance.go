package rules

import (
	"strings"
	"time"
)

// Category A — Conformance (OKF0xx). Always on, fixed at error (SPEC §9).

func init() {
	register(&Rule{
		ID: "OKF001", Name: "frontmatter-parseable", Category: Conformance,
		Default: Error, Check: checkOKF001,
	})
	register(&Rule{
		ID: "OKF002", Name: "type-required", Category: Conformance,
		Default: Error, Check: checkOKF002,
	})
	register(&Rule{
		ID: "OKF003", Name: "index-structure", Category: Conformance,
		Default: Error, Check: checkOKF003,
	})
	register(&Rule{
		ID: "OKF004", Name: "log-structure", Category: Conformance,
		Default: Error, Check: checkOKF004,
	})
}

// OKF001: every non-reserved .md carries a parseable YAML frontmatter block.
func checkOKF001(ctx *Context) []Finding {
	var fs []Finding
	for _, d := range ctx.Bundle.Concepts {
		if d.HasFrontmatter() {
			continue
		}
		var msg string
		switch {
		case !d.HasOpening:
			msg = "missing YAML frontmatter block"
		case !d.Terminated:
			msg = "unterminated frontmatter block (missing closing '---')"
		default:
			msg = "invalid YAML frontmatter: " + d.ParseErr.Error()
		}
		fs = append(fs, Finding{Path: d.Rel, Line: 1, Message: msg})
	}
	return fs
}

// OKF002: frontmatter contains a non-empty string `type`.
func checkOKF002(ctx *Context) []Finding {
	var fs []Finding
	for _, d := range ctx.Bundle.Concepts {
		if !d.HasFrontmatter() {
			continue // OKF001 already reports this
		}
		v, present := d.Frontmatter["type"]
		if !present {
			fs = append(fs, Finding{Path: d.Rel, Line: 1, Message: "frontmatter is missing a 'type' key"})
			continue
		}
		if s, ok := v.(string); !ok || strings.TrimSpace(s) == "" {
			fs = append(fs, Finding{Path: d.Rel, Line: 1, Message: "frontmatter 'type' must be a non-empty string"})
		}
	}
	return fs
}

// OKF003: index.md carries no frontmatter, except the bundle-root index.md which
// may carry only okf_version. (Body list structure is owned by OKF106.)
func checkOKF003(ctx *Context) []Finding {
	var fs []Finding
	for _, d := range ctx.Bundle.Indexes {
		if !d.HasOpening {
			continue
		}
		if !d.IsRootIndex() {
			fs = append(fs, Finding{Path: d.Rel, Line: 1, Message: "index.md must not carry frontmatter"})
			continue
		}
		if d.ParseErr != nil {
			fs = append(fs, Finding{Path: d.Rel, Line: 1, Message: "invalid YAML frontmatter: " + d.ParseErr.Error()})
			continue
		}
		for k := range d.Frontmatter {
			if k != "okf_version" {
				fs = append(fs, Finding{Path: d.Rel, Line: 1, Message: "root index.md frontmatter may only contain 'okf_version'; found '" + k + "'"})
			}
		}
	}
	return fs
}

// OKF004: log.md uses `## YYYY-MM-DD` headings, newest-first, and no frontmatter.
func checkOKF004(ctx *Context) []Finding {
	var fs []Finding
	for _, d := range ctx.Bundle.Logs {
		if d.HasOpening {
			fs = append(fs, Finding{Path: d.Rel, Line: 1, Message: "log.md must not carry frontmatter"})
		}
		var prev *time.Time
		for _, h := range d.Headings {
			if h.Level != 2 {
				continue // date entries are level-2 headings
			}
			ts, err := time.Parse("2006-01-02", strings.TrimSpace(h.Text))
			if err != nil || len(strings.TrimSpace(h.Text)) != 10 {
				fs = append(fs, Finding{Path: d.Rel, Line: h.Line, Message: "log heading '## " + h.Text + "' is not an ISO YYYY-MM-DD date"})
				continue
			}
			if prev != nil && ts.After(*prev) {
				fs = append(fs, Finding{Path: d.Rel, Line: h.Line, Message: "log entries must be newest-first; '" + h.Text + "' is newer than the entry above it"})
			}
			t := ts
			prev = &t
		}
	}
	return fs
}
