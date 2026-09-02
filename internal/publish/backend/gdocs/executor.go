package gdocs

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

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
			b.mu.Unlock()
		}
		return res, nil
	}

	if creating {
		b.mu.Lock()
		taken := b.titles
		b.mu.Unlock()
		title := disambiguate(tabTitle(rel, propOps), rel, taken)
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

	body, err := renderTab(mergedBlocks, mergedProps, r)
	if err != nil {
		return res, err
	}
	if err := b.writeTab(ctx, docID, tabID, rel, body); err != nil {
		return res, err
	}

	// The second pass. A heading's id is READ-ONLY and the batchUpdate reply does
	// not carry it (#150), so an anchor's real target can only be learned by
	// reading the document back after the write. This is the carve-out #152
	// accepted, and it is confined to tabs that actually host anchors.
	if len(body.anchorStarts) > 0 {
		ids, err := b.harvestHeadings(ctx, docID, tabID, body.anchorStarts)
		if err != nil {
			return res, err
		}
		for name, headingID := range ids {
			res.Anchors[name] = anchorID(tabID, headingID)
		}
	}
	return res, nil
}

// writeTab replaces a tab's whole body: drop the identity marker, clear the
// content, insert the new text, restyle it, and re-assert the marker.
//
// The marker is re-created rather than assumed to survive. The API documents only
// how a named range is ADJUSTED as content shifts, never what happens when the
// span it covers is deleted outright, and Google's own sample re-creates it after
// every rewrite (#158). Re-asserting makes the undocumented case moot.
func (b *Backend) writeTab(ctx context.Context, docID, tabID, rel string, body rendered) error {
	end, err := b.tabEndIndex(ctx, docID, tabID)
	if err != nil {
		return err
	}

	var requests []map[string]any

	// deleteNamedRange defaults to ALL TABS when tabsCriteria is omitted (#158), so
	// omitting it here would strip every other page's identity marker.
	requests = append(requests, map[string]any{
		"deleteNamedRange": map[string]any{
			"name":         namedRangeFor(rel),
			"tabsCriteria": map[string]any{"tabIds": []string{tabID}},
		},
	})
	if end > 2 {
		requests = append(requests, map[string]any{
			"deleteContentRange": map[string]any{
				"range": tabRange(tabID, 1, end-1),
			},
		})
	}

	const base = 1 // an emptied body's content begins at index 1
	if body.text != "" {
		requests = append(requests, map[string]any{
			"insertText": map[string]any{
				"location": map[string]any{"tabId": tabID, "index": base},
				"text":     body.text,
			},
		})
		requests = append(requests, shiftRequests(body.styles, base, tabID)...)
		requests = append(requests, map[string]any{
			"createNamedRange": map[string]any{
				"name":  namedRangeFor(rel),
				"range": tabRange(tabID, base, base+u16(body.text)),
			},
		})
	}

	if b.cfg.DryRunWriter != nil {
		return b.dumpRequests(docID, tabID, rel, requests)
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
func namedRangeFor(rel string) string { return "okf:" + rel }

// harvestHeadings reads the document back and matches each hosted anchor to the
// headingId of the paragraph it was rendered into.
func (b *Backend) harvestHeadings(ctx context.Context, docID, tabID string, starts map[publish.AnchorName]int) (map[publish.AnchorName]string, error) {
	doc, err := b.c.getDocument(ctx, docID)
	if err != nil {
		return nil, err
	}
	const base = 1
	byStart := map[int]string{}
	var walk func([]documentTab)
	walk = func(tabs []documentTab) {
		for _, t := range tabs {
			if t.TabProperties.TabID == tabID {
				for _, el := range t.DocumentTab.Body.Content {
					if el.Paragraph != nil && el.Paragraph.ParagraphStyle.HeadingID != "" {
						byStart[el.StartIndex] = el.Paragraph.ParagraphStyle.HeadingID
					}
				}
			}
			walk(t.ChildTabs)
		}
	}
	walk(doc.Tabs)

	out := map[publish.AnchorName]string{}
	for name, off := range starts {
		if id, ok := byStart[off+base]; ok {
			out[name] = id
		}
	}
	return out, nil
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

	replies, err := b.c.batchUpdate(ctx, docID, []map[string]any{
		{"addDocumentTab": map[string]any{"tabProperties": map[string]any{"title": title}}},
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

// tabEndIndex reports the end index of a tab's body content.
func (b *Backend) tabEndIndex(ctx context.Context, docID, tabID string) (int, error) {
	doc, err := b.c.getDocument(ctx, docID)
	if err != nil {
		return 0, err
	}
	var found int
	var walk func([]documentTab)
	walk = func(tabs []documentTab) {
		for _, t := range tabs {
			if t.TabProperties.TabID == tabID {
				for _, el := range t.DocumentTab.Body.Content {
					if el.EndIndex > found {
						found = el.EndIndex
					}
				}
			}
			walk(t.ChildTabs)
		}
	}
	walk(doc.Tabs)
	return found, nil
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

// dumpRequests writes the requests a run WOULD issue instead of issuing them.
//
// The interesting failure mode for this backend is "did I build the right
// batchUpdate", which an exported Markdown tree says nothing about — so the
// Docs-specific dry run dumps request payloads rather than rendered content.
func (b *Backend) dumpRequests(docID, tabID, rel string, requests []map[string]any) error {
	payload := map[string]any{
		"documentId": docID,
		"tabId":      tabID,
		"node":       rel,
		"requests":   requests,
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	_, err = b.cfg.DryRunWriter.Write(append(raw, '\n'))
	return err
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
