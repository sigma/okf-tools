package bundle

import (
	"sort"
	"strings"
	"unicode"
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

// Slug turns text into a fixed, GitHub-style anchor slug matching
// github-slugger byte-for-byte: trim surrounding whitespace, lowercase, drop
// every character but [a-z0-9], whitespace and hyphen, then map each whitespace
// character to a single hyphen — WITHOUT collapsing runs. Collapsing was the
// OKF203 divergence (okf-tools#116): GitHub does not collapse, so a "; — "-style
// gap where punctuation is stripped from between two spaces slugs to "--", and
// collapsing it to "-" produced false positives on GitHub-correct links. See the
// validated reference in sigma/ideas' retired sync/src/resolve.ts (ADR-0013).
// The algorithm is intentionally NOT configurable so it can't drift from a
// consumer (e.g. the Notion sync) that resolves the same anchors.
// Slug("Root KEK") == "root-kek", Slug("Foreign-rooted leaf") ==
// "foreign-rooted-leaf", Slug("TL;DR — the plan") == "tldr--the-plan".
//
// It is exported so the publishing backend's ScanRecompute re-derives anchor
// names from live glossary block text with the exact same normalization the
// bundle parser used — the shared algorithm that keeps both sides in lockstep.
func Slug(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(text)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case unicode.IsSpace(r):
			// Each whitespace char maps to one hyphen; runs are NOT collapsed,
			// matching github-slugger ("a & b" -> "a--b").
			b.WriteByte('-')
		}
	}
	return b.String()
}

// buildAnchors populates d.Anchors from its terms and headings, in file-line
// order, for a declared glossary file. Rules consult it to resolve #fragments
// and to detect slug collisions.
func buildAnchors(d *Doc) {
	anchors := make([]Anchor, 0, len(d.Terms)+len(d.Headings))
	for _, t := range d.Terms {
		anchors = append(anchors, Anchor{Slug: Slug(t.Text), Text: t.Text, Line: t.Line, Kind: AnchorTerm})
	}
	for _, h := range d.Headings {
		anchors = append(anchors, Anchor{Slug: Slug(h.Text), Text: h.Text, Line: h.Line, Kind: AnchorHeading})
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

// HasHeadingAnchor reports whether any heading in the doc GitHub-slugs to s.
// Unlike HasAnchor — which reads d.Anchors (terms + headings) and is populated
// only for declared glossary files — this is computed straight from the doc's
// headings, so it is meaningful for every doc, glossary or not. It is what
// OKF203 resolves a #heading fragment against on an ordinary page.
//
// Colliding heading slugs are not disambiguated (`-1`/`-2`): a bare #frag
// resolves iff some heading slugs to it, matching okftool's collisions-are-
// errors model (a plain GitHub-style slug, no per-render suffix).
func (d *Doc) HasHeadingAnchor(s string) bool {
	for _, h := range d.Headings {
		if Slug(h.Text) == s {
			return true
		}
	}
	return false
}
