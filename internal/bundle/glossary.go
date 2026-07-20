package bundle

import (
	"sort"
	"strings"
)

// AnchorKind distinguishes the two sources of a glossary anchor: a bold-lead
// term (CONTEXT-FORMAT entry) or a section heading.
type AnchorKind int

const (
	AnchorTerm AnchorKind = iota
	AnchorHeading
)

func (k AnchorKind) String() string {
	if k == AnchorHeading {
		return "heading"
	}
	return "term"
}

// Anchor is a defined, anchor-addressable target in a declared glossary file:
// the GitHub-style slug of a term or heading, plus the text and line it came
// from (for diagnostics). A glossary link's #fragment resolves iff it equals
// some Anchor's Slug.
type Anchor struct {
	Slug string
	Text string     // the raw term or heading text
	Line int        // 1-based line number in the glossary file
	Kind AnchorKind // term or heading
}

// slug turns text into a fixed, GitHub-style anchor slug: lowercase, drop every
// character but [a-z0-9], space and hyphen, map spaces to hyphens, then collapse
// runs of hyphens into one. The algorithm is intentionally NOT configurable so
// it can't drift from a consumer (e.g. the Notion sync) that resolves the same
// anchors. slug("Root KEK") == "root-kek", slug("Foreign-rooted leaf") ==
// "foreign-rooted-leaf".
func slug(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-':
			b.WriteByte('-')
		}
	}
	// Collapse runs of '-' (from spaces/hyphens) and trim the ends.
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return strings.Trim(out, "-")
}

// buildAnchors populates d.Anchors from its terms and headings, in file-line
// order, for a declared glossary file. Rules consult it to resolve #fragments
// and to detect slug collisions.
func buildAnchors(d *Doc) {
	anchors := make([]Anchor, 0, len(d.Terms)+len(d.Headings))
	for _, t := range d.Terms {
		anchors = append(anchors, Anchor{Slug: slug(t.Text), Text: t.Text, Line: t.Line, Kind: AnchorTerm})
	}
	for _, h := range d.Headings {
		anchors = append(anchors, Anchor{Slug: slug(h.Text), Text: h.Text, Line: h.Line, Kind: AnchorHeading})
	}
	sort.SliceStable(anchors, func(i, j int) bool { return anchors[i].Line < anchors[j].Line })
	d.Anchors = anchors
}

// HasAnchor reports whether the glossary doc defines the given slug (a term or
// heading). Non-glossary docs have no anchors and always return false.
func (d *Doc) HasAnchor(s string) bool {
	for _, a := range d.Anchors {
		if a.Slug == s {
			return true
		}
	}
	return false
}
