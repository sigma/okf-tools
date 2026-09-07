// Internal test package: these exercise the pure rendering helpers, which the
// end-to-end suite can only observe indirectly.
package gdocs

import (
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/graph"
)

// TestU16CountsCodeUnits pins the unit every Docs index is expressed in. Counting
// runes or bytes instead would misplace every style after the first non-BMP
// character — silently, and only for documents containing one.
func TestU16CountsCodeUnits(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"é", 1}, // BMP: one code unit, two UTF-8 bytes
		{"—", 1}, // em dash: one code unit, three bytes
		{"🐦", 2}, // outside the BMP: a surrogate PAIR
		{"a🐦b", 4},
	}
	for _, c := range cases {
		if got := u16(c.in); got != c.want {
			t.Errorf("u16(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestRenderTabOffsetsAreConsistent checks that every style range the renderer
// emits addresses text that actually exists — the invariant that makes the
// single-insert approach safe without the write-backwards technique.
func TestRenderTabOffsetsAreConsistent(t *testing.T) {
	blocks := []contentBlock{
		{kind: graph.Heading, level: 1, runs: []publish.Run{{Text: "Título 🐦"}}},
		{kind: graph.ListItem, level: 2, runs: []publish.Run{{Text: "nested"}}},
		{kind: graph.CodeBlock, runs: []publish.Run{{Text: "x := 1"}}},
	}
	body, err := renderTab(blocks, nil, nopResolver{})
	if err != nil {
		t.Fatalf("renderTab: %v", err)
	}
	total := u16(body.text)
	for _, req := range body.styles {
		for name, raw := range req {
			m := raw.(map[string]any)
			rng, ok := m["range"].(map[string]any)
			if !ok {
				continue
			}
			start, end := rng["startIndex"].(int), rng["endIndex"].(int)
			if start < 0 || end > total || start >= end {
				t.Errorf("%s range [%d,%d) is outside the body (length %d)", name, start, end, total)
			}
		}
	}
	// A nested list item carries its depth as leading tabs, which
	// createParagraphBullets counts and strips.
	if want := "\tnested\n"; !contains(body.text, want) {
		t.Errorf("nested list item lost its leading tab: %q", body.text)
	}
}

// TestAnchorBlockBecomesAHeading pins the mechanism the anchor design rests on:
// the only in-document link target available over REST is a heading, so a block
// hosting an anchor must be rendered as one.
func TestAnchorBlockBecomesAHeading(t *testing.T) {
	blocks := []contentBlock{
		{kind: graph.Paragraph, runs: []publish.Run{{Text: "Widget: a small thing."}},
			anchors: []publish.AnchorName{"glossary/widget"}},
	}
	body, err := renderTab(blocks, nil, nopResolver{})
	if err != nil {
		t.Fatalf("renderTab: %v", err)
	}
	if _, ok := body.anchorStarts["glossary/widget"]; !ok {
		t.Fatal("the anchor's paragraph start was not recorded")
	}
	var sawHeading bool
	for _, req := range body.styles {
		ups, ok := req["updateParagraphStyle"].(map[string]any)
		if !ok {
			continue
		}
		style := ups["paragraphStyle"].(map[string]any)
		if style["namedStyleType"] == "HEADING_6" {
			sawHeading = true
		}
	}
	if !sawHeading {
		t.Error("an anchor-hosting block was not rendered as a heading, so it has no link target")
	}
}

func TestDisambiguateTitles(t *testing.T) {
	taken := map[string]string{"Overview": "adr/overview.md"}
	if got := disambiguate("Overview", "adr/overview.md", taken); got != "Overview" {
		t.Errorf("a node must not disambiguate against itself: %q", got)
	}
	if got := disambiguate("Overview", "concepts/overview.md", taken); got != "concepts / Overview" {
		t.Errorf("collision not disambiguated by directory: %q", got)
	}
	if got := disambiguate("Overview", "overview.md", taken); got != "Overview" {
		t.Errorf("a root-level collision has no directory to qualify with: %q", got)
	}
	if got := disambiguate("Unique", "a/b.md", taken); got != "Unique" {
		t.Errorf("a non-colliding title must be left alone: %q", got)
	}
}

func TestHeadingStyleClamps(t *testing.T) {
	for in, want := range map[int]string{0: "HEADING_1", 1: "HEADING_1", 6: "HEADING_6", 9: "HEADING_6"} {
		if got := headingStyle(in); got != want {
			t.Errorf("headingStyle(%d) = %s, want %s", in, got, want)
		}
	}
}

// TestAnchorIDRoundTrip pins the packing that lets one BackendID carry both
// halves a HeadingLink needs.
func TestAnchorIDRoundTrip(t *testing.T) {
	id := anchorID("t.7", "h.3")
	tab, heading, ok := splitAnchorID(id)
	if !ok || tab != "t.7" || heading != "h.3" {
		t.Errorf("round trip lost data: %q -> %q %q %v", id, tab, heading, ok)
	}
	if _, _, ok := splitAnchorID(publish.BackendID("t.7")); ok {
		t.Error("a bare tab id must not parse as an anchor location")
	}
}

func TestTabTitlePrefersFrontmatter(t *testing.T) {
	props := []setProps{{props: map[string]any{"title": "Alpha"}}}
	if got := tabTitle("concepts/alpha.md", props); got != "Alpha" {
		t.Errorf("frontmatter title ignored: %q", got)
	}
	if got := tabTitle("concepts/alpha.md", nil); got != "alpha" {
		t.Errorf("fallback should be the file name: %q", got)
	}
}

// nopResolver resolves nothing; the render tests use runs without Refs.
type nopResolver struct{}

func (nopResolver) Resolve(publish.SymbolicID) (publish.BackendID, bool) { return "", false }

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (len(needle) == 0 || indexOf(hay, needle) >= 0)
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestBulletStripsShiftLaterOffsets is #185 at the unit: createParagraphBullets
// deletes the depth tabs it counts, so every offset the SAME batch uses after one
// must already be rebased onto the shorter body. The paragraph starts a hosted
// anchor is matched by move with it.
func TestBulletStripsShiftLaterOffsets(t *testing.T) {
	blocks := []contentBlock{
		{kind: graph.ListItem, level: 3, runs: []publish.Run{{Text: "deep"}}},
		{kind: graph.Paragraph, runs: []publish.Run{{Text: "Widget: a small thing."}},
			anchors: []publish.AnchorName{"glossary/widget"}},
		{kind: graph.Paragraph, runs: []publish.Run{{Text: "trailer"}}},
	}
	body, err := renderTab(blocks, nil, nopResolver{})
	if err != nil {
		t.Fatalf("renderTab: %v", err)
	}
	// Two tabs for level 3, and they are the only thing the batch removes.
	if body.strips != 2 {
		t.Errorf("strips = %d, want 2 (one per level below the first)", body.strips)
	}
	// "\t\tdeep\n" is 7 units as inserted and 5 once bulleted, so the anchor's
	// paragraph begins at 5, not 7.
	if got := body.anchorStarts["glossary/widget"]; got != 5 {
		t.Errorf("anchor paragraph start = %d, want 5 (the tabs are gone by then)", got)
	}
	var heading map[string]any
	for _, req := range body.styles {
		if ups, ok := req["updateParagraphStyle"].(map[string]any); ok {
			heading = ups["range"].(map[string]any)
		}
	}
	if heading == nil {
		t.Fatal("the anchor block emitted no heading style")
	}
	if heading["startIndex"] != 5 {
		t.Errorf("heading style starts at %v, want 5", heading["startIndex"])
	}
}

// TestOvershootIsRefusedNotClamped pins the diagnostic #185 asked for: a range
// that misses by more than the merged terminator means the renderer measured a
// body it did not produce, and silently clamping it would style the wrong text
// and hide the next occurrence. One unit is the boundary case; two is a fault.
func TestOvershootIsRefusedNotClamped(t *testing.T) {
	const total = 10
	styles := []styleReq{{shrink: 0, req: map[string]any{
		"createParagraphBullets": map[string]any{"range": relRange(2, total)},
	}}}
	if _, err := clampStyleRanges(styles, total); err != nil {
		t.Errorf("an end one past the last index is the terminator, and must clamp: %v", err)
	}
	if got := styles[0].req["createParagraphBullets"].(map[string]any)["range"].(map[string]any)["endIndex"]; got != total-1 {
		t.Errorf("end clamped to %v, want %d", got, total-1)
	}

	over := []styleReq{{shrink: 0, req: map[string]any{
		"updateTextStyle": map[string]any{"range": relRange(2, total+1)},
	}}}
	switch _, err := clampStyleRanges(over, total); {
	case err == nil:
		t.Error("a two-unit overshoot was accepted; it names a body the render never wrote")
	case !contains(err.Error(), "updateTextStyle") || !contains(err.Error(), "#185"):
		t.Errorf("the error does not name the offending request: %v", err)
	}
}

// TestStyleRangesStopShortOfTheTerminator pins the clamp: the last paragraph's
// terminator is not addressable, and a range that runs into it is answered with a
// 400 for the WHOLE atomic batch (#179, #185).
func TestStyleRangesStopShortOfTheTerminator(t *testing.T) {
	blocks := []contentBlock{{kind: graph.Quote, runs: []publish.Run{{Text: "last word"}}}}
	body, err := renderTab(blocks, nil, nopResolver{})
	if err != nil {
		t.Fatalf("renderTab: %v", err)
	}
	limit := u16(body.text) - 1
	for _, req := range body.styles {
		for name, raw := range req {
			rng := raw.(map[string]any)["range"].(map[string]any)
			if end := rng["endIndex"].(int); end > limit {
				t.Errorf("%s ends at %d, past the last addressable index %d", name, end, limit)
			}
		}
	}
}
