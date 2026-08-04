// Package fix is the mechanical autofix engine: the transforms that rewrite a
// bundle's pages to satisfy the fixable rules (link style, citation numbering,
// wikilink expansion, frontmatter order, timestamps) and regenerate index pages.
// It keys every transform off the single rules.FixKind vocabulary a rule declares,
// so a fixable rule can never be silently dropped. The command layer only decides
// which kinds to enable and where to render; the how lives here.
package fix

import (
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/config"
	"github.com/sigma/okf-tools/internal/rules"
	"gopkg.in/yaml.v3"
)

const maxInt = int(^uint(0) >> 1)

// Set is the set of mechanical transforms to apply, keyed by the single
// rules.FixKind vocabulary a rule declares. It only ever holds enabled kinds, so
// membership is has() and Any() is len > 0.
type Set map[rules.FixKind]bool

func (s Set) has(k rules.FixKind) bool { return s[k] }

// Any reports whether the set enables any transform.
func (s Set) Any() bool { return len(s) > 0 }

// Enabled collects the transforms of every rule that is enabled and selected —
// the single-sourced rule→transform map. Each rule contributes its declared Fix
// (when != FixNone); the engine keys off the same rules.FixKind vocabulary, so a
// fixable rule can never be silently dropped by forgetting to wire it here.
func Enabled(b *bundle.Bundle, selected, ignored map[string]bool) Set {
	set := Set{}
	for _, r := range rules.All() {
		switch {
		case r.Fix == rules.FixNone:
		case len(selected) > 0 && !selected[r.ID]:
		case ignored[r.ID]:
		case rules.Effective(r, b.Config) == rules.Off:
		default:
			set[r.Fix] = true
		}
	}
	return set
}

// pendingWrite is one file the set would rewrite: its rel (for reporting), path
// (for writing), and new content.
type pendingWrite struct {
	rel, path, content string
}

// pending returns every file the set would rewrite: concept docs whose fixDoc
// output differs from disk, plus regenerated indexes when FixIndex is set. Apply
// and Changed both derive from this, so "what Changed reports" and "what Apply
// writes" are the same list by construction.
func pending(b *bundle.Bundle, set Set) []pendingWrite {
	var out []pendingWrite
	for _, d := range b.Concepts {
		if nc := fixDoc(b, d, set); nc != d.Content {
			out = append(out, pendingWrite{d.Rel, d.Path, nc})
		}
	}
	if set.has(rules.FixIndex) {
		for _, idx := range b.Indexes {
			if nc := b.RenderIndex(idx); nc != idx.Content {
				out = append(out, pendingWrite{idx.Rel, idx.Path, nc})
			}
		}
	}
	return out
}

// Apply rewrites every file the set would change and returns the count written
// (the count so far if a write fails).
func Apply(b *bundle.Bundle, set Set) (int, error) {
	n := 0
	for _, w := range pending(b, set) {
		if err := os.WriteFile(w.path, []byte(w.content), 0o644); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Changed reports the bundle-relative paths the set would rewrite, without
// touching disk — the dry-run behind `fmt --check`. By the pending() invariant it
// is exactly the set of files Apply would write.
func Changed(b *bundle.Bundle, set Set) []string {
	writes := pending(b, set)
	rels := make([]string, len(writes))
	for i, w := range writes {
		rels[i] = w.rel
	}
	return rels
}

// fixDoc returns d's content with the selected transforms applied. Body edits
// preserve line count, so parser line numbers stay valid; the frontmatter block
// is rebuilt last.
func fixDoc(b *bundle.Bundle, d *bundle.Doc, set Set) string {
	lines := strings.Split(d.Content, "\n")
	bodyStart := d.BodyStartLine
	hasFM := d.HasOpening && d.Terminated

	var head, body []string
	if hasFM {
		head = append([]string(nil), lines[:bodyStart-1]...)
		body = append([]string(nil), lines[bodyStart-1:]...)
	} else {
		body = append([]string(nil), lines...)
	}

	if set.has(rules.FixLinkStyle) {
		for _, rl := range d.Resolved {
			if rl.Class != bundle.ClassConcept {
				continue
			}
			bi := rl.Line - bodyStart
			if bi < 0 || bi >= len(body) {
				continue
			}
			if nt, ok := restyle(b, d, rl); ok {
				body[bi] = strings.Replace(body[bi], "]("+rl.Target+")", "]("+nt+")", 1)
			}
		}
	}
	if set.has(rules.FixCitations) {
		renumberCitations(b, d, body, bodyStart)
	}
	if set.has(rules.FixWikilinks) {
		body = rewriteWikilinks(b, d, body)
	}
	if hasFM && (set.has(rules.FixFrontmatter) || set.has(rules.FixTimestamp)) {
		if nh, ok := fixFrontmatterHead(b, d, set); ok {
			head = nh
		}
	}

	if hasFM {
		return strings.Join(head, "\n") + "\n" + strings.Join(body, "\n")
	}
	return strings.Join(body, "\n")
}

func restyle(b *bundle.Bundle, d *bundle.Doc, rl bundle.ResolvedLink) (string, bool) {
	p, frag := splitFrag(rl.Target)
	switch b.Config.Links.Style {
	case "relative":
		if rl.Absolute {
			targetRel := strings.TrimPrefix(p, "/")
			return bundle.RelSlash(path.Dir(d.Rel), targetRel) + frag, true
		}
	case "absolute":
		if !rl.Absolute && rl.Inside {
			return "/" + b.Rel(rl.Resolved) + frag, true
		}
	}
	return "", false
}

var wikilinkSpanRe = regexp.MustCompile(`\[\[([^\]|]+)(?:\|([^\]]*))?\]\]`)

// rewriteWikilinks replaces unambiguous [[target]] / [[target|label]] with a
// standard markdown link; ambiguous or unknown targets are left untouched.
func rewriteWikilinks(b *bundle.Bundle, d *bundle.Doc, body []string) []string {
	joined := strings.Join(body, "\n")
	joined = wikilinkSpanRe.ReplaceAllStringFunc(joined, func(m string) string {
		sub := wikilinkSpanRe.FindStringSubmatch(m)
		target, label := sub[1], sub[1]
		if sub[2] != "" {
			label = sub[2]
		}
		p, frag := splitFrag(target)
		t := b.ResolveWikilink(p)
		if t == nil {
			return m
		}
		return "[" + label + "](" + b.LinkURL(d.Rel, t) + frag + ")"
	})
	return strings.Split(joined, "\n")
}

var (
	citEntryRe         = regexp.MustCompile(`^\[(\d+)\]\s+\[[^\]]*\]\([^)]*\)`)
	citNumRe           = regexp.MustCompile(`^(\s*)\[\d+\]`)
	citEntryFootnoteRe = regexp.MustCompile(`^\[\^(\d+)\]:\s+\[[^\]]*\]\([^)]*\)`)
	citNumFootnoteRe   = regexp.MustCompile(`^(\s*)\[\^\d+\]:`)
)

func renumberCitations(b *bundle.Bundle, d *bundle.Doc, body []string, bodyStart int) {
	start, end := citationRange(d, b.Config)
	if start == 0 {
		return
	}
	entryRe, numRe := citEntryRe, citNumRe
	repl := func(n int) string { return "${1}[" + strconv.Itoa(n) + "]" }
	if b.Config.Citations.Style == "footnote" {
		entryRe, numRe = citEntryFootnoteRe, citNumFootnoteRe
		repl = func(n int) string { return "${1}[^" + strconv.Itoa(n) + "]:" }
	}
	n := 0
	for bi := 0; bi < len(body); bi++ {
		fileLine := bodyStart + bi
		if fileLine < start || fileLine >= end {
			continue
		}
		if entryRe.MatchString(strings.TrimSpace(body[bi])) {
			n++
			body[bi] = numRe.ReplaceAllString(body[bi], repl(n))
		}
	}
}

func citationRange(d *bundle.Doc, cfg *config.Config) (start, end int) {
	want := strings.ToLower(strings.TrimSpace(strings.TrimLeft(cfg.Citations.Heading, "# ")))
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
	end = maxInt
	for _, h := range d.Headings {
		if h.Line > hLine && h.Level <= hLevel && h.Line < end {
			end = h.Line
		}
	}
	return hLine + 1, end
}

// fixFrontmatterHead rebuilds the frontmatter block (including delimiters) with
// canonical key order and/or a normalized timestamp.
func fixFrontmatterHead(b *bundle.Bundle, d *bundle.Doc, set Set) ([]string, bool) {
	format := b.Config.Frontmatter.TimestampFormat
	var raw string
	var ok bool
	if set.has(rules.FixFrontmatter) {
		raw, ok = reorderFrontmatter(d.FrontmatterKey, set.has(rules.FixTimestamp), format)
	} else if set.has(rules.FixTimestamp) {
		raw, ok = normalizeTimestampOnly(d.FrontmatterRaw, format)
	}
	if !ok {
		return nil, false
	}
	raw = strings.TrimRight(raw, "\n")
	head := append([]string{"---"}, strings.Split(raw, "\n")...)
	head = append(head, "---")
	return head, true
}

var canonicalKeys = []string{"type", "title", "description", "resource", "tags", "timestamp"}

func reorderFrontmatter(node *yaml.Node, normTS bool, format string) (string, bool) {
	if node == nil || node.Kind != yaml.MappingNode {
		return "", false
	}
	type kv struct{ k, v *yaml.Node }
	present := map[string]kv{}
	var origOrder []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		present[k.Value] = kv{k, v}
		origOrder = append(origOrder, k.Value)
	}
	var content []*yaml.Node
	used := map[string]bool{}
	emit := func(key string) {
		p := present[key]
		if key == "timestamp" && normTS {
			normalizeNode(p.v, format)
		}
		content = append(content, p.k, p.v)
		used[key] = true
	}
	for _, key := range canonicalKeys {
		if _, ok := present[key]; ok {
			emit(key)
		}
	}
	for _, key := range origOrder {
		if !used[key] {
			emit(key)
		}
	}
	out, err := yaml.Marshal(&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: content})
	if err != nil {
		return "", false
	}
	return string(out), true
}

func normalizeNode(v *yaml.Node, format string) {
	if s, ok := normalizeTimestampValue(v.Value, format); ok {
		v.Value = s
		v.Tag = ""
		v.Style = 0
	}
}

var tsLineRe = regexp.MustCompile(`^(\s*timestamp:\s*)(.*)$`)

func normalizeTimestampOnly(fmRaw, format string) (string, bool) {
	lines := strings.Split(fmRaw, "\n")
	changed := false
	for i, l := range lines {
		m := tsLineRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if nv, ok := normalizeTimestampValue(m[2], format); ok && nv != stripQuotes(strings.TrimSpace(m[2])) {
			lines[i] = m[1] + nv
			changed = true
		}
	}
	return strings.Join(lines, "\n"), changed
}

func normalizeTimestampValue(raw, format string) (string, bool) {
	t, ok := parseAnyTimestamp(stripQuotes(strings.TrimSpace(raw)))
	if !ok {
		return "", false
	}
	switch format {
	case "date":
		return t.Format("2006-01-02"), true
	case "rfc3339":
		return t.Format(time.RFC3339), true
	}
	return "", false
}

func parseAnyTimestamp(v string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02", time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func splitFrag(t string) (pathPart, frag string) {
	if i := strings.IndexByte(t, '#'); i >= 0 {
		return t[:i], t[i:]
	}
	return t, ""
}

func stripQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
