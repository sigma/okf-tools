// Package convention owns the authoring conventions okftool both detects and
// repairs: the citation grammar and the frontmatter timestamp layouts.
//
// It exists because detect and repair are two halves of one contract. The rule
// catalog (internal/rules) reports a malformed or misnumbered citation; the fix
// engine (internal/fix) rewrites it. When each package encodes the grammar
// itself, the two can disagree about what a citation *is* — lint flagging a line
// fmt will not repair, or fmt rewriting a line lint considers fine. Before this
// package, the citation-entry patterns were byte-identical copies in
// internal/rules/helpers.go and internal/fix/fix.go, the citations-section walk
// was written out in both, and the timestamp layout lists were maintained
// separately.
//
// One module, so detect and repair read the same match.
package convention

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/config"
)

// Citation-entry patterns. The numbered form is `[n] [label](target)`; the
// footnote form (citations.style = "footnote") is `[^n]: [label](target)`. In
// both, group 1 is the citation number; the marker patterns are the looser
// in-body proxy for "cites sources".
var (
	entryRe          = regexp.MustCompile(`^\[(\d+)\]\s+\[[^\]]*\]\([^)]*\)`)
	entryFootnoteRe  = regexp.MustCompile(`^\[\^(\d+)\]:\s+\[[^\]]*\]\([^)]*\)`)
	numRe            = regexp.MustCompile(`^(\s*)\[\d+\]`)
	numFootnoteRe    = regexp.MustCompile(`^(\s*)\[\^\d+\]:`)
	markerRe         = regexp.MustCompile(`(?m)^\s*\[\d+\]\s`)
	markerFootnoteRe = regexp.MustCompile(`\[\^\d+\]`)
)

// Citations is one bundle's citation grammar: the entry shape its configured
// style expects, and where in a document the citations section sits.
type Citations struct {
	footnote bool
	heading  string
}

// CitationsFor reads the grammar a bundle's config selects.
func CitationsFor(cfg *config.Config) Citations {
	return Citations{
		footnote: cfg.Citations.Style == "footnote",
		heading:  cfg.Citations.Heading,
	}
}

// Entry reports whether line is a well-formed citation entry, and the number it
// carries. The caller is expected to have trimmed surrounding space.
func (c Citations) Entry(line string) (n int, ok bool) {
	re := entryRe
	if c.footnote {
		re = entryFootnoteRe
	}
	m := re.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// Renumber rewrites a citation entry's marker to n, preserving the line's
// leading whitespace. A line Entry does not match is returned unchanged.
func (c Citations) Renumber(line string, n int) string {
	if c.footnote {
		return numFootnoteRe.ReplaceAllString(line, "${1}[^"+strconv.Itoa(n)+"]:")
	}
	return numRe.ReplaceAllString(line, "${1}["+strconv.Itoa(n)+"]")
}

// Example renders the expected entry shape, for diagnostics.
func (c Citations) Example() string {
	if c.footnote {
		return "[^n]: [label](target)"
	}
	return "[n] [label](target)"
}

// HasMarkers reports whether a document body carries reference markers — the
// numbered `[1] ...` line form, or `[^1]` footnote markers — the mechanical
// proxy for "cites sources".
func (c Citations) HasMarkers(body string) bool {
	if c.footnote {
		return markerFootnoteRe.MatchString(body)
	}
	return markerRe.MatchString(body)
}

// Section returns the file line range [start, end) covered by the citations
// section of d — the lines after its heading, up to the next heading of the
// same or shallower level. start is 0 when d has no citations heading.
func (c Citations) Section(d *bundle.Doc) (start, end int) {
	want := strings.ToLower(strings.TrimSpace(strings.TrimLeft(c.heading, "# ")))
	hLine, hLevel := 0, 0
	for _, h := range d.Headings {
		if strings.ToLower(strings.TrimSpace(h.Text)) == want {
			hLine, hLevel = h.Line, h.Level
			break
		}
	}
	if hLine == 0 {
		return 0, 0
	}
	end = int(^uint(0) >> 1)
	for _, h := range d.Headings {
		if h.Line > hLine && h.Level <= hLevel && h.Line < end {
			end = h.Line
		}
	}
	return hLine + 1, end
}

// SectionLines returns the raw content lines of d's citations section and the
// file line number of the first one.
func (c Citations) SectionLines(d *bundle.Doc) (startLine int, lines []string, found bool) {
	start, end := c.Section(d)
	if start == 0 {
		return 0, nil, false
	}
	content := strings.Split(d.Content, "\n")
	for ln := start; ln < end && ln-1 < len(content); ln++ {
		lines = append(lines, strings.TrimRight(content[ln-1], "\r"))
	}
	return start, lines, true
}
