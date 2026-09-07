package gdocs

import (
	"context"
	"fmt"
	"path"
	"strings"
	"unicode"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// maxTabs is the documented ceiling on tabs per document.
//
// It is stated only in end-user help, not in the API reference, and no error code
// is documented for exceeding it (#147) — so this backend enforces it itself,
// with a message naming the selection and the count. Splitting into a second
// document would invent an identity key, a second NotebookLM source slot, and an
// arbitrary boundary through the middle of an area; truncating would be silent
// data loss (#155).
const maxTabs = 100

// bodyBase is where a tab's body begins once it has been emptied: index 1, since
// index 0 is the segment start the API does not let a caller write at.
//
// Every relative offset the renderer produces — a style range, a hosted anchor's
// paragraph start, a self-link's text span — is turned absolute by adding this,
// and the harvest matches paragraphs back by subtracting it. One constant so the
// four sites cannot drift apart and style the wrong span.
const bodyBase = 1

// Execute applies one sealed transaction as a batchUpdate against the
// destination document. The unit of work is a TAB, so a transaction creates a
// tab, replaces its content, or deletes it.
func (b *Backend) Execute(ctx context.Context, txn publish.Transaction, r backend.Resolver) (publish.ExecResult, error) {
	t, ok := txn.(*Transaction)
	if !ok {
		return publish.ExecResult{}, fmt.Errorf("gdocs: foreign transaction type %T", txn)
	}
	rel, err := relOf(t.group)
	if err != nil {
		return publish.ExecResult{}, err
	}

	b.mu.Lock()
	docID := b.docID
	tabID := b.tabs[rel]
	b.mu.Unlock()
	if docID == "" {
		return publish.ExecResult{}, fmt.Errorf("gdocs: execute before provision")
	}

	res := publish.ExecResult{
		Nodes:   map[publish.SymbolicID]publish.BackendID{},
		Anchors: map[publish.AnchorName]publish.BackendID{},
	}

	// Sort the transaction's units into the three things a tab can be asked to do.
	// Reading the props unit BEFORE the create matters: the tab's title comes from
	// frontmatter, and Execute never sees the NodeStamp that carries it.
	var (
		blocks   []contentBlock
		propOps  []setProps
		creating bool
		deleting bool
		createID publish.SymbolicID
	)
	for _, u := range t.units {
		switch p := u.payload.(type) {
		case createTab:
			creating, createID = true, p.node
		case deleteTab:
			deleting = true
		case setProps:
			propOps = append(propOps, p)
		case contentBlock:
			blocks = append(blocks, p)
		}
	}

	if deleting {
		if tabID != "" {
			if err := b.deleteTab(ctx, docID, tabID); err != nil {
				return res, err
			}
			b.mu.Lock()
			delete(b.tabs, rel)
			// Drop its remembered citations too, or a later re-link would style a
			// range in a tab that no longer exists.
			delete(b.citations, tabID)
			// And release its title: the scan seeds the claimed-title map from the
			// document, so a title deleted during this run would otherwise stay
			// claimed and push a later node with the same name into a needless
			// directory prefix (#180).
			for title, owner := range b.titles {
				if owner == rel {
					delete(b.titles, title)
				}
			}
			b.mu.Unlock()
		}
		return res, nil
	}

	if creating {
		b.mu.Lock()
		taken := b.titles
		b.mu.Unlock()
		// Fit BEFORE disambiguating as well as after: the claimed-title map holds
		// fitted titles, so comparing a raw one against it would miss every clash
		// between titles that differ only past the ceiling — exactly the titles most
		// likely to collide once truncated (#178). The outer fit then re-bounds the
		// directory prefix disambiguate may have added.
		title := fitTabTitle(disambiguate(fitTabTitle(tabTitle(rel, propOps)), rel, taken))
		id, err := b.createTab(ctx, docID, rel, title)
		if err != nil {
			return res, err
		}
		tabID = id
		res.Nodes[createID] = publish.BackendID(id)
	}
	if len(blocks) == 0 && len(propOps) == 0 {
		return res, nil
	}
	if tabID == "" {
		return res, fmt.Errorf("gdocs: no tab for %s; a create must precede its content", rel)
	}

	// Merge this transaction's parts into whatever earlier transactions of the same
	// node already contributed, and re-render the whole tab from the merged state.
	b.mu.Lock()
	pend, ok := b.pending[rel]
	if !ok {
		pend = &pendingTab{}
		b.pending[rel] = pend
	}
	pend.props = append(pend.props, propOps...)
	pend.blocks = append(pend.blocks, blocks...)
	mergedProps := append([]setProps(nil), pend.props...)
	mergedBlocks := append([]contentBlock(nil), pend.blocks...)
	b.mu.Unlock()

	// A transaction's own anchors resolve against a transaction-local overlay, the
	// mechanism WithOverlay exists for. This backend cannot supply a real id there
	// — a headingId is minted by the server and readable only from a document read
	// (#150) — so the overlay resolves them to a deferral sentinel: the citation
	// renders, the transaction is not rejected for an "unresolvable" ref (#170),
	// and its link target is applied below, once harvested.
	body, err := renderTab(mergedBlocks, mergedProps,
		backend.WithOverlay(r, deferredAnchors(mergedBlocks, tabID)))
	if err != nil {
		return res, err
	}
	if err := b.writeTab(ctx, docID, tabID, rel, body); err != nil {
		return res, err
	}

	// The second pass, and the reason this backend has one: two facts about a tab
	// exist only AFTER the write, and neither can be predicted.
	//
	// A heading's id is read-only and the batchUpdate reply does not carry it
	// (#150), so an anchor's target is learned by reading back — the carve-out
	// #152 accepted. Where the body actually ENDS is the same kind of fact: the
	// arithmetic prediction was wrong by one (#179). One read answers both, and
	// one batch acts on both.
	if err := b.assertTabState(ctx, docID, tabID, rel, body, res.Anchors); err != nil {
		return res, err
	}
	// This tab's own citations are re-rendered from scratch on every write, so the
	// remembered set is REPLACED rather than extended: the previous write's ranges
	// describe text that no longer exists.
	b.rememberCitations(tabID, body.citations, res.Anchors)
	if err := b.relinkMoved(ctx, docID, res.Anchors); err != nil {
		return res, err
	}
	return res, nil
}

// assertTabState is the post-write pass: read the tab back once, then issue the
// single batch that needs what the read revealed — the identity marker over the
// body's REAL extent, and the link targets for citations of anchors this tab
// hosts. Neither could be known before the write: a heading id is minted by the
// server (#170's deferral rests on that), and the body's real extent contradicted
// the arithmetic that predicted it (#179).
func (b *Backend) assertTabState(ctx context.Context, docID, tabID, rel string,
	body rendered, anchors map[publish.AnchorName]publish.BackendID) error {
	if body.text == "" {
		// Nothing was written, so there is no marker to place and no heading to
		// harvest. Skipping keeps a props-only or delete transaction free of the
		// read this pass otherwise costs.
		return nil
	}

	var state tabState
	if b.cfg.DryRunWriter == nil {
		var err error
		if state, err = b.readTabState(ctx, docID, tabID); err != nil {
			return err
		}
		for name, headingID := range state.anchorHeadings(body.anchorStarts) {
			anchors[name] = anchorID(tabID, headingID)
		}
	} else {
		// A dry run has nothing to read back, but the anchors must still RESOLVE:
		// the transport gates every citing transaction on them, so leaving them out
		// would stall the plan and the dump would show only part of the run. The
		// marker range is modelled rather than read: the trailing newline of a
		// rendered body merges with the paragraph terminator, so the segment grows
		// one unit less than the text (#179). It is the best a dump can do, and a
		// model is exactly what a dry run cannot verify — which is why one could
		// never have caught this.
		for name := range body.anchorStarts {
			anchors[name] = anchorID(tabID, b.nextDryRunHeadingID())
		}
		// The bullet requests shorten the body by every depth tab they consume
		// (#185), so the modelled segment has to lose them too.
		state.end = bodyBase + u16(body.text) - body.strips
		if strings.HasSuffix(body.text, "\n") {
			state.end--
		}
	}

	var requests []map[string]any
	{
		// The marker is re-created on every write rather than assumed to survive:
		// the API documents only how a named range is ADJUSTED as content shifts,
		// never what happens when the span it covers is deleted outright, and
		// Google's own sample re-creates it (#158).
		requests = append(requests, map[string]any{
			"createNamedRange": map[string]any{
				"name":  namedRangeFor(rel),
				"range": tabRange(tabID, bodyBase, state.contentEnd()),
			},
		})
	}
	links, err := b.selfRefRequests(tabID, body.citations, anchors)
	if err != nil {
		return err
	}
	requests = append(requests, links...)
	if len(requests) == 0 {
		return nil
	}
	_, err = b.c.batchUpdate(ctx, docID, requests)
	return err
}

// rememberCitations records where this tab cites each anchor, so a later
// re-minting of that anchor can be repaired without re-rendering (#171).
//
// A deferred citation is recorded too, with the target it was just linked to:
// once its own tab is written, it is an ordinary citation that some future
// rewrite of this same tab could strand.
func (b *Backend) rememberCitations(tabID string, cites []anchorCitation,
	hosted map[publish.AnchorName]publish.BackendID) {
	placed := make([]anchorCitation, 0, len(cites))
	for _, c := range cites {
		if c.deferred() {
			id, ok := hosted[c.name]
			if !ok {
				continue // never linked, so there is nothing to repair later
			}
			c.target = id
		}
		c.start, c.end = c.start+bodyBase, c.end+bodyBase
		placed = append(placed, c)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(placed) == 0 {
		delete(b.citations, tabID)
		return
	}
	b.citations[tabID] = placed
}

// relinkMoved repairs every remembered citation whose target has moved.
//
// A tab's body is rewritten in full on each of its transactions, which deletes
// and re-inserts its paragraphs, so the server mints FRESH heading ids every time
// (#150). Any link another tab was already given to an old id still renders — it
// just goes nowhere. Re-styling the affected ranges is the repair; the text is
// untouched, so no id moves again as a result.
//
// A run in which nothing moved issues no request at all, which is the common case
// and keeps the 60-writes-per-minute budget (#156) intact.
func (b *Backend) relinkMoved(ctx context.Context, docID string,
	minted map[publish.AnchorName]publish.BackendID) error {
	if len(minted) == 0 {
		return nil
	}

	// These ranges do NOT go through shiftRequests: that shift converts a fresh
	// render's relative offsets, and a remembered citation was shifted into the
	// document's frame when it was recorded. tabRange still stamps the tab id,
	// which is the half of shiftRequests that must never be skipped (#147).
	//
	// moved names the citations this batch repairs, so their remembered targets can
	// be updated AFTER the write lands. Recording them up front would claim a
	// repair that a failed batch never made, and the next harvest would see the
	// citation as current and skip it forever.
	type moved struct {
		tab   string
		index int
		to    publish.BackendID
	}

	b.mu.Lock()
	var requests []map[string]any
	var repaired []moved
	for tabID, cites := range b.citations {
		for i, c := range cites {
			now, ok := minted[c.name]
			if !ok || now == c.target {
				continue
			}
			hostTab, headingID, ok := splitAnchorID(now)
			if !ok {
				// The harvest matched no paragraph for this anchor, so there is no target
				// to point at. Leaving the citation on its previous id is no worse than
				// the state before this write, and the anchor's own gate reports it.
				continue
			}
			requests = append(requests, map[string]any{"updateTextStyle": map[string]any{
				"range":     tabRange(tabID, c.start, c.end),
				"textStyle": map[string]any{"link": headingLink(hostTab, headingID)},
				"fields":    "link",
			}})
			repaired = append(repaired, moved{tab: tabID, index: i, to: now})
		}
	}
	b.mu.Unlock()

	if len(requests) == 0 {
		return nil
	}
	if _, err := b.c.batchUpdate(ctx, docID, requests); err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range repaired {
		if cites, ok := b.citations[m.tab]; ok && m.index < len(cites) {
			cites[m.index].target = m.to
		}
	}
	return nil
}

// nextDryRunHeadingID mints a distinct placeholder per dry-run write, so a
// rewrite of the same tab looks like the re-minting it really is and the dump
// plans the re-linking a real run would issue (#171).
func (b *Backend) nextDryRunHeadingID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dryHeadingCount++
	return fmt.Sprintf("%s-%d", dryRunHeadingID, b.dryHeadingCount)
}

// selfRefRequests builds the link styles that could not exist during the write:
// one per citation of an anchor this tab hosts, over the range its text already
// occupies.
//
// Styling text already in place is what keeps the ids valid: a rewrite deletes
// and re-inserts the paragraphs, so the server mints fresh headingIds and the
// ones just harvested go dead.
//
// This is the same-tab half of the repair. A later transaction for the same node
// re-renders and rewrites the whole tab (see Execute's merge of b.pending) and
// re-mints its heading ids; self-citations survive that because they are
// re-emitted from every render, and links other tabs hold are repaired by
// relinkMoved (#171).
func (b *Backend) selfRefRequests(tabID string, cites []anchorCitation,
	anchors map[publish.AnchorName]publish.BackendID) ([]map[string]any, error) {
	var requests []map[string]any
	for _, sl := range cites {
		if !sl.deferred() {
			continue // an ordinary citation; it linked during the write
		}
		// These two are loud rather than skipped. A deferred citation names an
		// anchor THIS transaction hosts, so the harvest that just ran should have
		// produced it; "the anchor's own gate will report it" is true for another
		// tab's citation and false here, where silence would publish a citation
		// that links nowhere and say nothing.
		id, ok := anchors[sl.name]
		if !ok {
			return nil, fmt.Errorf(
				"gdocs: anchor %s is hosted here but no heading was harvested for it", sl.name)
		}
		_, headingID, ok := splitAnchorID(id)
		if !ok {
			return nil, fmt.Errorf("gdocs: anchor %s resolved to a malformed target %q", sl.name, id)
		}
		requests = append(requests, map[string]any{"updateTextStyle": map[string]any{
			"range":     relRange(sl.start, sl.end),
			"textStyle": map[string]any{"link": headingLink(tabID, headingID)},
			"fields":    "link",
		}})
	}
	// The same relative-to-absolute shift the body's own styles go through, for
	// the same reason: an untagged range silently applies to the FIRST tab (#147).
	return shiftRequests(requests, bodyBase, tabID), nil
}

// writeTab replaces a tab's whole body: drop the identity marker, clear the
// content, insert the new text and restyle it. The identity marker is asserted
// afterwards, by assertTabState, because its range depends on where the body
// really ended up (#179).
//
// The marker is re-created rather than assumed to survive. The API documents only
// how a named range is ADJUSTED as content shifts, never what happens when the
// span it covers is deleted outright, and Google's own sample re-creates it after
// every rewrite (#158). Re-asserting makes the undocumented case moot.
func (b *Backend) writeTab(ctx context.Context, docID, tabID, rel string, body rendered) error {
	// A dry run reads too — reading mutates nothing, and the dump should describe
	// the requests a real run would issue against THIS destination. The one case
	// it cannot read is a destination that does not exist yet, where there is no
	// document to ask about and every tab is one the run would create.
	b.mu.Lock()
	missing := b.missing
	b.mu.Unlock()

	var state tabState
	if !missing {
		var err error
		if state, err = b.readTabState(ctx, docID, tabID); err != nil {
			return err
		}
	}
	var requests []map[string]any
	var err error

	// Delete the identity marker only if it is THERE. Deleting an absent named
	// range is a hard 400, not a no-op, and batchUpdate is atomic — so an
	// unguarded delete failed every first publish into a fresh destination while
	// every rewrite stayed green (#176). This is the same shape as the content
	// guard below: touch what exists.
	//
	// tabsCriteria is not optional: deleteNamedRange defaults to ALL TABS when it
	// is omitted (#158), which would strip every other page's marker.
	if state.hasNamedRange(namedRangeFor(rel)) {
		requests = append(requests, map[string]any{
			"deleteNamedRange": map[string]any{
				"name":         namedRangeFor(rel),
				"tabsCriteria": map[string]any{"tabIds": []string{tabID}},
			},
		})
	}
	if state.end > 2 {
		requests = append(requests, map[string]any{
			"deleteContentRange": map[string]any{
				"range": tabRange(tabID, 1, state.end-1),
			},
		})
	}

	if body.text != "" {
		requests = append(requests, map[string]any{
			"insertText": map[string]any{
				"location": map[string]any{"tabId": tabID, "index": bodyBase},
				"text":     body.text,
			},
		})
		requests = append(requests, shiftRequests(body.styles, bodyBase, tabID)...)
	}

	_, err = b.c.batchUpdate(ctx, docID, requests)
	return err
}

// shiftRequests turns relative offsets into absolute document indexes and stamps
// the tab id onto every range.
//
// The tab id is not optional politeness: a request that omits it silently applies
// to the FIRST tab of the document (#147), so a missing one would quietly style —
// or overwrite — the wrong page.
func shiftRequests(reqs []map[string]any, base int, tabID string) []map[string]any {
	out := make([]map[string]any, 0, len(reqs))
	for _, req := range reqs {
		for _, body := range req {
			m, ok := body.(map[string]any)
			if !ok {
				continue
			}
			rng, ok := m["range"].(map[string]any)
			if !ok {
				continue
			}
			rng["startIndex"] = rng["startIndex"].(int) + base
			rng["endIndex"] = rng["endIndex"].(int) + base
			rng["tabId"] = tabID
		}
		out = append(out, req)
	}
	return out
}

func tabRange(tabID string, start, end int) map[string]any {
	return map[string]any{"tabId": tabID, "startIndex": start, "endIndex": end}
}

// namedRangeFor is a node's identity marker: stable across renames because it is
// keyed by the source path, and 1–256 code units as the API requires (#158).
func namedRangeFor(rel string) string { return markerPrefix + rel }

// markerPrefix distinguishes this backend's identity ranges from any other named
// range a human may have left in the document.
const markerPrefix = "okf:"

// relOfMarker inverts namedRangeFor: it recovers the node a named range
// identifies, so a tab can be recognised from the DOCUMENT rather than from
// external state (#180).
func relOfMarker(name string) (string, bool) {
	rel, ok := strings.CutPrefix(name, markerPrefix)
	return rel, ok && rel != ""
}

// anchorHeadings matches each hosted anchor to the headingId of the paragraph it
// was rendered into.
func (s tabState) anchorHeadings(starts map[publish.AnchorName]int) map[publish.AnchorName]string {
	out := map[publish.AnchorName]string{}
	for name, off := range starts {
		if id, ok := s.headings[off+bodyBase]; ok {
			out[name] = id
		}
	}
	return out
}

// createTab adds a tab and returns its server-minted id.
//
// A brand-new document already has one empty default tab, so the FIRST page
// adopts it — retitling rather than adding — instead of leaving a stray "Tab 1"
// in every published document.
//
// parentTabId is never set: tabs are flat (#155). The parent Ref the optimizer
// stamped is still consumed by the transport's readiness gate, so ordering holds.
func (b *Backend) createTab(ctx context.Context, docID, rel, title string) (string, error) {
	if adopt := b.takeAdoptableTab(); adopt != "" {
		if _, err := b.c.batchUpdate(ctx, docID, []map[string]any{
			{"updateDocumentTabProperties": map[string]any{
				"tabProperties": map[string]any{"tabId": adopt, "title": title},
				"fields":        "title",
			}},
		}); err != nil {
			return "", fmt.Errorf("gdocs: adopt default tab for %s: %w", rel, err)
		}
		b.recordTab(rel, adopt, title)
		return adopt, nil
	}

	b.mu.Lock()
	count := len(b.tabs)
	b.mu.Unlock()
	if count >= maxTabs {
		return "", fmt.Errorf("gdocs: selection %q needs more than %d tabs, the documented "+
			"per-document ceiling; split the area rather than the document",
			b.cfg.Selection, maxTabs)
	}

	// An area's landing README opens its document. Placement is asserted rather
	// than inherited: tabs are created in sorted-GroupKey order, so the overview
	// would otherwise land wherever its path happens to sort (#163).
	props := map[string]any{"title": title}
	if b.isAreaRoot(rel) {
		props["index"] = 0
	}
	replies, err := b.c.batchUpdate(ctx, docID, []map[string]any{
		{"addDocumentTab": map[string]any{"tabProperties": props}},
	})
	if err != nil {
		return "", fmt.Errorf("gdocs: create tab for %s: %w", rel, err)
	}
	id := tabIDFromReply(replies)
	if id == "" {
		return "", fmt.Errorf("gdocs: create tab for %s: reply carried no tabId", rel)
	}
	b.recordTab(rel, id, title)
	return id, nil
}

// isAreaRoot reports whether rel is the landing README of the area this document
// publishes. The backend knows its own selection, which is the area's key or
// path, so it can recognise the page without an areas.json of its own.
func (b *Backend) isAreaRoot(rel string) bool {
	if path.Base(rel) != "README.md" {
		return false
	}
	sel := strings.Trim(b.cfg.Selection, "/")
	if sel == "" || sel == "." {
		// A whole-bundle document has no single area to open with.
		return false
	}
	return path.Dir(rel) == sel
}

func (b *Backend) recordTab(rel, id, title string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tabs[rel] = id
	b.titles[title] = rel
}

// takeAdoptableTab consumes the document's default tab, if it is still unclaimed.
func (b *Backend) takeAdoptableTab() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.adoptable
	b.adoptable = ""
	return id
}

// deleteTab removes a tab. It CASCADES to child tabs, which is safe only because
// tabs are flat.
func (b *Backend) deleteTab(ctx context.Context, docID, tabID string) error {
	_, err := b.c.batchUpdate(ctx, docID, []map[string]any{
		{"deleteTab": map[string]any{"tabId": tabID}},
	})
	return err
}

// tabState is what one document read tells writeTab about the tab it is about to
// rewrite: where its content ends, and which named ranges it already carries.
//
// Both facts gate a delete, and both come from the SAME read — asking the API
// again for the second one would cost a round trip per tab for information the
// first response already carried (#176).
type tabState struct {
	// end is the end index of the tab's body content; 0 for a tab with no body.
	end int
	// namedRanges holds the names the tab carries, so a marker is deleted only if
	// it is there to delete. It is nil for the unread cases — a fresh destination,
	// or the zero value returned with an error — and a nil map reads as "carries
	// nothing", which is exactly right for a tab that does not exist yet.
	namedRanges map[string]bool
	// headings maps a paragraph's start index to the headingId the server minted
	// for it. Read-only server state, and the reason a post-write read exists at
	// all (#150).
	headings map[int]string
}

// contentEnd is the exclusive end a range may address: one before the end the
// read reported, which is the last position inside the segment.
//
// It is only meaningful for a tab that HAS a body; assertTabState returns before
// calling it when nothing was written.
//
// Predicting this from the text that was SENT is what #179 got wrong. A rendered
// body ends in "\n", and that newline merges with the paragraph terminator the
// segment already has, so the segment grows by one unit fewer than the string —
// a createNamedRange predicted at 19088 against a segment ending at 19087.
// Reading it back needs no theory about what the server normalizes.
func (s tabState) contentEnd() int { return s.end - 1 }

// hasNamedRange reports whether the tab already carries the named range.
func (s tabState) hasNamedRange(name string) bool { return s.namedRanges[name] }

// readTabState reads the document and reports the state of one tab.
func (b *Backend) readTabState(ctx context.Context, docID, tabID string) (tabState, error) {
	doc, err := b.c.getDocument(ctx, docID)
	if err != nil {
		return tabState{}, err
	}
	out := tabState{namedRanges: map[string]bool{}, headings: map[int]string{}}
	var walk func([]documentTab)
	walk = func(tabs []documentTab) {
		for _, t := range tabs {
			if t.TabProperties.TabID == tabID {
				for _, el := range t.DocumentTab.Body.Content {
					if el.EndIndex > out.end {
						out.end = el.EndIndex
					}
					if el.Paragraph != nil && el.Paragraph.ParagraphStyle.HeadingID != "" {
						out.headings[el.StartIndex] = el.Paragraph.ParagraphStyle.HeadingID
					}
				}
				for name := range t.DocumentTab.NamedRanges {
					out.namedRanges[name] = true
				}
			}
			walk(t.ChildTabs)
		}
	}
	walk(doc.Tabs)
	return out, nil
}

// tabTitle picks a tab's title: the frontmatter title where the run asserts one,
// else the file's name.
func tabTitle(rel string, props []setProps) string {
	for _, p := range props {
		if t, ok := p.props["title"].(string); ok && t != "" {
			return t
		}
	}
	return strings.TrimSuffix(path.Base(rel), ".md")
}

// maxTabTitle is the API's ceiling on a tab title. An over-long title is
// rejected outright rather than truncated server-side, and batchUpdate is
// atomic — so one long title used to fail its whole transaction, and the
// selection with it (#178).
//
// The error says "characters"; this counts UTF-16 code units, the unit every
// other index in this API is expressed in. Where the two differ — an emoji, an
// astral-plane character — counting units is the conservative reading: it
// truncates a little early rather than sending a title the server may still
// reject.
const maxTabTitle = 50

// fitTabTitle brings a title within the ceiling, marking it as shortened.
//
// A descriptive, sentence-shaped title is normal in an OKF bundle rather than a
// tail case, so this fires often. It cuts to 49 characters plus an ellipsis
// instead of a hard 50-character chop, because a bare chop mid-word reads as a
// corrupted title rather than a deliberate one.
//
// Truncation is safe BECAUSE identity never runs through the title: a document
// is found by its appProperties key and a tab by its named range (#151), and
// nothing looks a tab up by title. Two titles truncating to the same string is
// therefore cosmetic, not an identity collision, and needs no uniquing — the
// same reason disambiguate is free to rewrite a title too.
//
// It runs AFTER disambiguate, whose directory prefix can itself push a title
// over the ceiling.
func fitTabTitle(title string) string {
	if u16(title) <= maxTabTitle {
		return title
	}
	// Cut on a RUNE boundary at or under the unit ceiling, so the result is never
	// a split surrogate pair — which would be invalid text, not a short title.
	kept := make([]rune, 0, maxTabTitle)
	used := 0
	for _, r := range title {
		n := u16(string(r))
		if used+n > maxTabTitle-1 { // -1 leaves room for the ellipsis
			break
		}
		kept = append(kept, r)
		used += n
	}
	// Trailing whitespace before the ellipsis reads as a typo rather than a cut.
	return strings.TrimRightFunc(string(kept), unicode.IsSpace) + "…"
}

// disambiguate qualifies a title with its parent directory when another page in
// the same document already claimed it. Duplicate titles across directories are
// normal, so a collision must not fail a publish — and nothing looks a tab up BY
// title, since identity is the tabId.
func disambiguate(title, rel string, taken map[string]string) string {
	other, clash := taken[title]
	if !clash || other == rel {
		return title
	}
	dir := path.Dir(rel)
	if dir == "." || dir == "" {
		return title
	}
	return path.Base(dir) + " / " + title
}

// tabIDFromReply digs the minted tabId out of an addDocumentTab reply.
func tabIDFromReply(replies []map[string]any) string {
	for _, rep := range replies {
		add, ok := rep["addDocumentTab"].(map[string]any)
		if !ok {
			continue
		}
		props, ok := add["tabProperties"].(map[string]any)
		if !ok {
			continue
		}
		if id, ok := props["tabId"].(string); ok {
			return id
		}
	}
	return ""
}
