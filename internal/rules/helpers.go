package rules

import (
	"regexp"
	"strings"
	"time"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/config"
	"github.com/sigma/okf-tools/internal/parser"
	"gopkg.in/yaml.v3"
)

const maxInt = int(^uint(0) >> 1)

// fmScalar returns the literal scalar value (as written) of a top-level
// frontmatter key, using the order-preserving yaml.Node so the original text
// survives (yaml would otherwise reparse e.g. a date into a time.Time).
func fmScalar(node *yaml.Node, key string) (val string, found bool) {
	if node == nil {
		return "", false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1].Value, true
		}
	}
	return "", false
}

var kebabRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// isKebabName reports whether a filename's stem is lowercase-hyphenated.
func isKebabName(name string) bool {
	return kebabRe.MatchString(strings.TrimSuffix(name, ".md"))
}

// unambiguousWikilinkTarget returns the single concept a wikilink names, or nil
// if zero or many match. Delegates to the shared bundle resolver.
func unambiguousWikilinkTarget(b *bundle.Bundle, target string) *bundle.Doc {
	return b.ResolveWikilink(target)
}

// citationSectionLines returns the raw content lines of the citations section
// and the file line number of the first such line.
func citationSectionLines(d *bundle.Doc, cfg *config.Config) (startLine int, lines []string, found bool) {
	want := strings.ToLower(strings.TrimSpace(strings.TrimLeft(cfg.Citations.Heading, "# ")))
	var h *parser.Heading
	for i := range d.Headings {
		if strings.ToLower(strings.TrimSpace(d.Headings[i].Text)) == want {
			h = &d.Headings[i]
			break
		}
	}
	if h == nil {
		return 0, nil, false
	}
	end := maxInt
	for _, nh := range d.Headings {
		if nh.Line > h.Line && nh.Level <= h.Level && nh.Line < end {
			end = nh.Line
		}
	}
	content := strings.Split(d.Content, "\n")
	startLine = h.Line + 1
	for ln := startLine; ln < end && ln-1 < len(content); ln++ {
		lines = append(lines, strings.TrimRight(content[ln-1], "\r"))
	}
	return startLine, lines, true
}

// Citation-entry regexes. The numbered form is `[n] [label](target)`; the
// footnote form (citations.style = "footnote") is `[^n]: [label](target)`.
var (
	citationLineRe           = regexp.MustCompile(`^\[(\d+)\]\s+\[[^\]]*\]\([^)]*\)`)
	citationLineFootnoteRe   = regexp.MustCompile(`^\[\^(\d+)\]:\s+\[[^\]]*\]\([^)]*\)`)
	citationMarkerRe         = regexp.MustCompile(`(?m)^\s*\[\d+\]\s`)
	citationMarkerFootnoteRe = regexp.MustCompile(`\[\^\d+\]`)
)

// citationEntryRe returns the citation-entry regex for the configured style;
// group 1 is the citation number in both.
func citationEntryRe(cfg *config.Config) *regexp.Regexp {
	if cfg.Citations.Style == "footnote" {
		return citationLineFootnoteRe
	}
	return citationLineRe
}

// citationEntryExample renders the expected entry shape for diagnostics.
func citationEntryExample(cfg *config.Config) string {
	if cfg.Citations.Style == "footnote" {
		return "[^n]: [label](target)"
	}
	return "[n] [label](target)"
}

// hasCitationMarkers reports whether the body carries reference markers — the
// numbered `[1] ...` line form, or `[^1]` footnote markers — a mechanical proxy
// for "cites sources".
func hasCitationMarkers(d *bundle.Doc, cfg *config.Config) bool {
	if cfg.Citations.Style == "footnote" {
		return citationMarkerFootnoteRe.MatchString(d.Body)
	}
	return citationMarkerRe.MatchString(d.Body)
}

// matchesTimestamp reports whether a literal timestamp matches the format.
func matchesTimestamp(val, format string) bool {
	val = strings.TrimSpace(val)
	switch format {
	case "date":
		_, err := time.Parse("2006-01-02", val)
		return err == nil && len(val) == 10
	case "rfc3339":
		if _, err := time.Parse(time.RFC3339, val); err == nil {
			return true
		}
		_, err := time.Parse(time.RFC3339Nano, val)
		return err == nil
	}
	return true // unknown format: do not flag
}

// parseableTimestamp reports whether a value can be understood well enough to be
// normalized by autofix.
func parseableTimestamp(val string) bool {
	val = strings.TrimSpace(val)
	for _, f := range []string{"2006-01-02", time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04:05Z07:00"} {
		if _, err := time.Parse(f, val); err == nil {
			return true
		}
	}
	return false
}

func formatLabel(format string) string {
	switch format {
	case "date":
		return "a YYYY-MM-DD date"
	case "rfc3339":
		return "an RFC3339 datetime"
	}
	return format
}
