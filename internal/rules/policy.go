package rules

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/config"
)

// Category B — Policy (OKF1xx). Configurable; defaults are spec-aligned.

func init() {
	register(&Rule{
		ID: "OKF101", Name: "no-wikilinks", Category: Policy,
		Default: Warning, Fixable: true,
		Enabled: func(c *config.Config) bool { return !c.Links.AllowWikilinks },
		Check:   checkOKF101,
	})
	register(&Rule{
		ID: "OKF102", Name: "link-style", Category: Policy,
		Default: Warning, Fixable: true,
		Enabled: func(c *config.Config) bool {
			return c.Links.Style == "relative" || c.Links.Style == "absolute"
		},
		Check: checkOKF102,
	})
	register(&Rule{
		ID: "OKF103", Name: "filename-case", Category: Policy,
		Default:   Info,
		SevConfig: func(c *config.Config) string { return c.Filenames.Severity },
		Check:     checkOKF103,
	})
	register(&Rule{
		ID: "OKF104", Name: "timestamp-format", Category: Policy,
		Default: Warning, Fixable: true, Check: checkOKF104,
	})
	register(&Rule{
		ID: "OKF105", Name: "citations-format", Category: Policy,
		Default: Warning, Fixable: true, Check: checkOKF105,
	})
	register(&Rule{
		ID: "OKF106", Name: "index-sync", Category: Policy,
		Default: Warning, Fixable: true,
		Enabled: func(c *config.Config) bool { return c.Index.CheckSync },
		Check:   checkOKF106,
	})
	register(&Rule{
		ID: "OKF107", Name: "description-present", Category: Policy,
		Default: Info,
		Enabled: func(c *config.Config) bool { return c.Frontmatter.RequireDescription },
		Check:   checkOKF107,
	})
}

// OKF101: no Obsidian [[wiki-links]] (they are not standard markdown).
func checkOKF101(ctx *Context) []Finding {
	var fs []Finding
	for _, d := range ctx.Bundle.Docs {
		for _, l := range d.Links {
			if !l.Wikilink {
				continue
			}
			fixable := unambiguousWikilinkTarget(ctx.Bundle, l.Target) != nil
			fs = append(fs, Finding{
				Path: d.Rel, Line: l.Line, Fixable: fixable,
				Message: "Obsidian wiki-link '[[" + l.Target + "]]'; use a standard markdown link",
			})
		}
	}
	return fs
}

// OKF102: concept cross-links match the configured style.
func checkOKF102(ctx *Context) []Finding {
	var fs []Finding
	style := ctx.Config.Links.Style
	for _, d := range ctx.Bundle.Concepts {
		for _, rl := range d.Resolved {
			if rl.Class != bundle.ClassConcept {
				continue
			}
			switch style {
			case "relative":
				if rl.Absolute {
					fs = append(fs, Finding{Path: d.Rel, Line: rl.Line, Fixable: true,
						Message: "bundle-absolute link '" + rl.Target + "'; this bundle requires relative links"})
				} else if !rl.Inside {
					fs = append(fs, Finding{Path: d.Rel, Line: rl.Line, Fixable: false,
						Message: "relative link '" + rl.Target + "' escapes the bundle root"})
				}
			case "absolute":
				if !rl.Absolute {
					fs = append(fs, Finding{Path: d.Rel, Line: rl.Line, Fixable: rl.Inside,
						Message: "relative link '" + rl.Target + "'; this bundle requires bundle-absolute links"})
				}
			}
		}
	}
	return fs
}

// OKF103: concept filenames match the configured case convention.
func checkOKF103(ctx *Context) []Finding {
	if ctx.Config.Filenames.Case != "kebab" {
		return nil // only kebab is implemented; other values are a no-op
	}
	var fs []Finding
	for _, d := range ctx.Bundle.Concepts {
		if !isKebabName(d.Base) {
			fs = append(fs, Finding{Path: d.Rel, Line: 0,
				Message: "filename '" + d.Base + "' is not kebab-case (lowercase, hyphen-separated)"})
		}
	}
	return fs
}

// OKF104: a present `timestamp` matches the configured format; absence is only a
// problem when require_timestamp is set.
func checkOKF104(ctx *Context) []Finding {
	var fs []Finding
	format := ctx.Config.Frontmatter.TimestampFormat
	for _, d := range ctx.Bundle.Concepts {
		if !d.HasFrontmatter() {
			continue
		}
		if _, present := d.Frontmatter["timestamp"]; !present {
			if ctx.Config.Frontmatter.RequireTimestamp {
				fs = append(fs, Finding{Path: d.Rel, Line: 1, Message: "frontmatter is missing a required 'timestamp'"})
			}
			continue
		}
		val, _ := fmScalar(d.FrontmatterKey, "timestamp")
		if !matchesTimestamp(val, format) {
			fs = append(fs, Finding{Path: d.Rel, Line: 1, Fixable: parseableTimestamp(val),
				Message: "timestamp '" + val + "' is not " + formatLabel(format)})
		}
	}
	return fs
}

// OKF105: sources under the citations heading are numbered `[n] [label](target)`.
func checkOKF105(ctx *Context) []Finding {
	var fs []Finding
	for _, d := range ctx.Bundle.Concepts {
		start, lines, found := citationSectionLines(d, ctx.Config)
		if !found {
			if ctx.Config.Citations.RequireWhenCited && hasCitationMarkers(d) {
				fs = append(fs, Finding{Path: d.Rel, Line: 1,
					Message: "citation markers present but no '" + ctx.Config.Citations.Heading + "' section"})
			}
			continue
		}
		expected := 1
		for i, raw := range lines {
			line := strings.TrimSpace(raw)
			if line == "" {
				continue
			}
			m := citationLineRe.FindStringSubmatch(line)
			if m == nil {
				fs = append(fs, Finding{Path: d.Rel, Line: start + i,
					Message: "malformed citation; expected '[n] [label](target)'"})
				continue
			}
			n, _ := strconv.Atoi(m[1])
			if n != expected {
				fs = append(fs, Finding{Path: d.Rel, Line: start + i, Fixable: true,
					Message: fmt.Sprintf("citation numbered [%d]; expected [%d]", n, expected)})
			}
			expected++
		}
	}
	return fs
}

// OKF106: each index enumerates exactly the concepts in its scope, with no dead
// entries and (optionally) frontmatter-matching descriptions.
func checkOKF106(ctx *Context) []Finding {
	var fs []Finding
	descs := ctx.Config.Index.DescriptionsFromFrontmatter
	for _, idx := range ctx.Bundle.Indexes {
		scope := ctx.Bundle.Scope(idx)
		want := make(map[string]*bundle.Doc, len(scope))
		for _, c := range scope {
			want[c.Rel] = c
		}
		have := map[string]bool{}
		for _, e := range ctx.Bundle.IndexEntries(idx) {
			if e.Target == nil {
				fs = append(fs, Finding{Path: idx.Rel, Line: e.Line, Fixable: true,
					Message: "index entry '" + e.Title + "' does not resolve to a concept in the bundle"})
				continue
			}
			have[e.Rel] = true
			if _, ok := want[e.Rel]; !ok {
				fs = append(fs, Finding{Path: idx.Rel, Line: e.Line, Fixable: true,
					Message: "index lists '" + e.Rel + "', which is not in this index's scope"})
			} else if descs {
				if wantDesc := e.Target.Description(); wantDesc != "" && e.Desc != wantDesc {
					fs = append(fs, Finding{Path: idx.Rel, Line: e.Line, Fixable: true,
						Message: "index description for '" + e.Rel + "' does not match its frontmatter"})
				}
			}
		}
		for _, c := range scope {
			if !have[c.Rel] {
				fs = append(fs, Finding{Path: idx.Rel, Line: 0, Fixable: true,
					Message: "index is missing an entry for '" + c.Rel + "'"})
			}
		}
	}
	return fs
}

// OKF107: concept frontmatter has a non-empty description.
func checkOKF107(ctx *Context) []Finding {
	var fs []Finding
	for _, d := range ctx.Bundle.Concepts {
		if !d.HasFrontmatter() {
			continue
		}
		s, ok := d.Frontmatter["description"].(string)
		if !ok || strings.TrimSpace(s) == "" {
			fs = append(fs, Finding{Path: d.Rel, Line: 1, Message: "frontmatter is missing a non-empty 'description'"})
		}
	}
	return fs
}
