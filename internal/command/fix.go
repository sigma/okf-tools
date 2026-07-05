package command

import (
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/config"
	"gopkg.in/yaml.v3"
)

const maxInt = int(^uint(0) >> 1)

// fixOptions selects which mechanical transforms to apply.
type fixOptions struct {
	Wikilinks   bool // OKF101: [[x]] -> [x](x.md) when unambiguous
	LinkStyle   bool // OKF102: restyle concept cross-links
	Timestamp   bool // OKF104: normalize frontmatter timestamp
	Citations   bool // OKF105: renumber citation entries
	Frontmatter bool // fmt: canonical frontmatter key order
	Index       bool // OKF106: regenerate index.md files
}

func (o fixOptions) any() bool {
	return o.Wikilinks || o.LinkStyle || o.Timestamp || o.Citations || o.Frontmatter || o.Index
}

// applyFixes rewrites every changed file in the bundle and returns the count.
func applyFixes(b *bundle.Bundle, opts fixOptions) (int, error) {
	changed := 0
	for _, d := range b.Concepts {
		nc := fixDoc(b, d, opts)
		if nc != d.Content {
			if err := os.WriteFile(d.Path, []byte(nc), 0o644); err != nil {
				return changed, err
			}
			changed++
		}
	}
	if opts.Index {
		for _, idx := range b.Indexes {
			nc := b.RenderIndex(idx)
			if nc != idx.Content {
				if err := os.WriteFile(idx.Path, []byte(nc), 0o644); err != nil {
					return changed, err
				}
				changed++
			}
		}
	}
	return changed, nil
}

// fixDoc returns d's content with the selected transforms applied. Body edits
// preserve line count, so parser line numbers stay valid; the frontmatter block
// is rebuilt last.
func fixDoc(b *bundle.Bundle, d *bundle.Doc, opts fixOptions) string {
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

	if opts.LinkStyle {
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
	if opts.Citations {
		renumberCitations(b, d, body, bodyStart)
	}
	if opts.Wikilinks {
		body = rewriteWikilinks(b, d, body)
	}
	if hasFM && (opts.Frontmatter || opts.Timestamp) {
		if nh, ok := fixFrontmatterHead(b, d, opts); ok {
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
	citEntryRe = regexp.MustCompile(`^\[(\d+)\]\s+\[[^\]]*\]\([^)]*\)`)
	citNumRe   = regexp.MustCompile(`^(\s*)\[\d+\]`)
)

func renumberCitations(b *bundle.Bundle, d *bundle.Doc, body []string, bodyStart int) {
	start, end := citationRange(d, b.Config)
	if start == 0 {
		return
	}
	n := 0
	for bi := 0; bi < len(body); bi++ {
		fileLine := bodyStart + bi
		if fileLine < start || fileLine >= end {
			continue
		}
		if citEntryRe.MatchString(strings.TrimSpace(body[bi])) {
			n++
			body[bi] = citNumRe.ReplaceAllString(body[bi], "${1}["+strconv.Itoa(n)+"]")
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
func fixFrontmatterHead(b *bundle.Bundle, d *bundle.Doc, opts fixOptions) ([]string, bool) {
	format := b.Config.Frontmatter.TimestampFormat
	var raw string
	var ok bool
	if opts.Frontmatter {
		raw, ok = reorderFrontmatter(d.FrontmatterKey, opts.Timestamp, format)
	} else if opts.Timestamp {
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
