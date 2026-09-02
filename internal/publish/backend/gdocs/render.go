package gdocs

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/graph"
)

// anchorHeadingLevel is the heading level an anchor-hosting block is rendered at.
//
// Anchors need a link TARGET, and the only in-document targets the API offers are
// bookmarks — which cannot be created over REST — and headings (#150). So a block
// that declares an anchor is rendered as a heading, which both gives it a
// headingId and puts the term in the document outline. A glossary term is a
// heading in every sense that matters; this is the mechanism the decision assumed.
const anchorHeadingLevel = 6

// rendered is one tab's whole body: the text to insert, the styling requests that
// decorate it, and where each hosted anchor's paragraph begins.
//
// Offsets are UTF-16 code units relative to the START of the inserted text, and
// are turned into absolute document indexes by the caller. Building the body as
// ONE insert plus offset-addressed styling sidesteps index drift entirely — the
// "write backwards" technique (#150) is for interleaved inserts, and there are
// none here.
type rendered struct {
	text string
	// styles are requests whose ranges are relative; shiftRequests makes them absolute.
	styles []map[string]any
	// anchorStarts maps an anchor to the relative start offset of its paragraph, so
	// the read-back pass can match it to a headingId.
	anchorStarts map[publish.AnchorName]int
}

// u16 counts a string's length in UTF-16 code units — the unit every Docs index
// is expressed in. A surrogate pair (an emoji) counts 2, so counting runes or
// bytes would silently misplace every style after the first one.
func u16(s string) int { return len(utf16.Encode([]rune(s))) }

// renderTab turns a transaction's ordered blocks into one tab body.
func renderTab(blocks []contentBlock, props []setProps, r backend.Resolver) (rendered, error) {
	out := rendered{anchorStarts: map[publish.AnchorName]int{}}
	var sb strings.Builder

	// Properties lead the tab as plain text. A tab has no property surface of its
	// own — the carve-out from #152 — so frontmatter is rendered rather than asserted.
	for _, p := range props {
		for _, k := range sortedKeys(p.props) {
			fmt.Fprintf(&sb, "%s: %v\n", k, p.props[k])
		}
	}

	for _, blk := range blocks {
		start := u16(sb.String())
		text, styles, err := renderBlockText(blk, start, r)
		if err != nil {
			return out, err
		}
		sb.WriteString(text)
		out.styles = append(out.styles, styles...)
		for _, a := range blk.anchors {
			out.anchorStarts[a] = start
		}
	}
	out.text = sb.String()
	return out, nil
}

// renderBlockText renders one block and the styling that decorates it. start is
// the block's offset within the tab body.
func renderBlockText(blk contentBlock, start int, r backend.Resolver) (string, []map[string]any, error) {
	// A table's content lives per-cell, not in runs.
	if blk.kind == graph.Table {
		return renderTableText(blk), nil, nil
	}

	prefix := ""
	if blk.kind == graph.ListItem && blk.level > 1 {
		// Nesting is expressed by LEADING TABS, which createParagraphBullets counts
		// and strips (#150) — there is no explicit depth field.
		prefix = strings.Repeat("\t", blk.level-1)
	}

	body, linkStyles, err := renderRuns(blk.runs, start+u16(prefix), r)
	if err != nil {
		return "", nil, err
	}
	text := prefix + body + "\n"
	end := start + u16(text)

	styles := linkStyles
	switch {
	case len(blk.anchors) > 0:
		// An anchor-hosting block becomes a heading so it has a link target.
		styles = append(styles, paragraphStyle(start, end, map[string]any{
			"namedStyleType": headingStyle(anchorHeadingLevel),
		}, "namedStyleType"))
	case blk.kind == graph.Heading:
		styles = append(styles, paragraphStyle(start, end, map[string]any{
			"namedStyleType": headingStyle(blk.level),
		}, "namedStyleType"))
	case blk.kind == graph.ListItem:
		styles = append(styles, map[string]any{
			"createParagraphBullets": map[string]any{
				"range":        relRange(start, end),
				"bulletPreset": "BULLET_DISC_CIRCLE_SQUARE",
			},
		})
	case blk.kind == graph.CodeBlock:
		// There is no native code block (#150). Monospace plus a shaded, bordered
		// paragraph is the documented degradation.
		styles = append(styles,
			map[string]any{"updateTextStyle": map[string]any{
				"range":     relRange(start, end),
				"textStyle": map[string]any{"weightedFontFamily": map[string]any{"fontFamily": "Courier New"}},
				"fields":    "weightedFontFamily",
			}},
			paragraphStyle(start, end, map[string]any{
				"shading": map[string]any{"backgroundColor": map[string]any{
					"color": map[string]any{"rgbColor": map[string]any{"red": 0.95, "green": 0.95, "blue": 0.95}},
				}},
			}, "shading"))
	case blk.kind == graph.Quote:
		styles = append(styles, paragraphStyle(start, end, map[string]any{
			"indentStart": map[string]any{"magnitude": 36, "unit": "PT"},
		}, "indentStart"))
	}
	return text, styles, nil
}

// renderRuns renders a block's inline runs and the link styling over them.
//
// A Ref run carries NO visible text — the label is the backend's to supply — so a
// node reference shows its page name and an anchor reference its term.
func renderRuns(runs []publish.Run, start int, r backend.Resolver) (string, []map[string]any, error) {
	resolved, err := backend.ResolveRuns(runs, r)
	if err != nil {
		return "", nil, err
	}
	var sb strings.Builder
	var styles []map[string]any
	at := start

	for _, rr := range resolved {
		text := rr.Run.Text
		var link map[string]any

		switch {
		case rr.Run.Ref != "":
			if name, isAnchor := rr.Run.Ref.AnchorName(); isAnchor {
				text = anchorLabel(name)
				tabID, headingID, ok := splitAnchorID(rr.RefID)
				if ok {
					link = map[string]any{"heading": map[string]any{"id": headingID, "tabId": tabID}}
				}
			} else {
				text = nodeLabel(rr.Run.Ref)
				// Link.tabId is the internal link to another tab of this document (#150).
				link = map[string]any{"tabId": string(rr.RefID)}
			}
		case rr.Run.Link != "":
			link = map[string]any{"url": rr.Run.Link}
		}
		if text == "" {
			continue
		}
		next := at + u16(text)
		if link != nil {
			styles = append(styles, map[string]any{"updateTextStyle": map[string]any{
				"range":     relRange(at, next),
				"textStyle": map[string]any{"link": link},
				"fields":    "link",
			}})
		}
		sb.WriteString(text)
		at = next
	}
	return sb.String(), styles, nil
}

// renderTableText renders a table as text rows.
//
// This is a DELIBERATE degradation, not an omission. insertTable creates
// structure whose per-cell indexes are only knowable from a read-back, turning
// one table into an extra round-trip and a fragile index calculation; and the fidelity
// floor for this backend is "readable structure and self-describing prose",
// because the primary consumer reduces a Doc to text anyway (#148). A pipe-
// separated row keeps every cell and the reading order.
func renderTableText(blk contentBlock) string {
	var sb strings.Builder
	for _, row := range blk.rows {
		cells := make([]string, 0, len(row))
		for _, cell := range row {
			var cs strings.Builder
			for _, run := range cell {
				if run.Ref != "" {
					if name, ok := run.Ref.AnchorName(); ok {
						cs.WriteString(anchorLabel(name))
						continue
					}
					cs.WriteString(nodeLabel(run.Ref))
					continue
				}
				cs.WriteString(run.Text)
			}
			cells = append(cells, strings.TrimSpace(cs.String()))
		}
		sb.WriteString(strings.Join(cells, " | "))
		sb.WriteString("\n")
	}
	return sb.String()
}

// --- small helpers ----------------------------------------------------------

func relRange(start, end int) map[string]any {
	return map[string]any{"startIndex": start, "endIndex": end}
}

func paragraphStyle(start, end int, style map[string]any, fields string) map[string]any {
	return map[string]any{"updateParagraphStyle": map[string]any{
		"range":          relRange(start, end),
		"paragraphStyle": style,
		"fields":         fields,
	}}
}

func headingStyle(level int) string {
	if level < 1 {
		level = 1
	}
	if level > 6 {
		level = 6
	}
	return fmt.Sprintf("HEADING_%d", level)
}

// nodeLabel is the visible text of a link to another page: its file name without
// the extension.
func nodeLabel(id publish.SymbolicID) string {
	rel := id.Rel()
	return strings.TrimSuffix(path.Base(rel), ".md")
}

// anchorLabel is the visible text of a glossary citation: the term's slug,
// stripped of its role namespace.
func anchorLabel(name publish.AnchorName) string {
	s := string(name)
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// anchorID packs an anchor's location into one BackendID.
//
// A HeadingLink needs BOTH the heading id and the tab it lives in, but the
// resolution table stores a single opaque BackendID per anchor — so the tab rides
// along in the value rather than being looked up separately.
func anchorID(tabID, headingID string) publish.BackendID {
	return publish.BackendID(tabID + "#" + headingID)
}

func splitAnchorID(id publish.BackendID) (tabID, headingID string, ok bool) {
	tabID, headingID, ok = strings.Cut(string(id), "#")
	return tabID, headingID, ok && tabID != "" && headingID != ""
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
