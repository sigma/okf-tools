package gdocs

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
)

// Execute applies one sealed transaction as a batchUpdate against the
// destination document, resolving late-bound Refs as it renders.
//
// The unit of work is a TAB, so a transaction touches exactly one node: create
// its tab, replace its content, or delete it.
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

	var (
		body     strings.Builder
		hosted   []publish.AnchorName
		hasWrite bool
	)

	for _, u := range t.units {
		switch p := u.payload.(type) {
		case createTab:
			id, err := b.createTab(ctx, docID, rel)
			if err != nil {
				return res, err
			}
			tabID = id
			res.Nodes[p.node] = publish.BackendID(id)

		case deleteTab:
			if tabID == "" {
				continue // nothing to remove; a scan that saw no tab already dropped it
			}
			if err := b.deleteTab(ctx, docID, tabID); err != nil {
				return res, err
			}
			b.mu.Lock()
			delete(b.tabs, rel)
			b.mu.Unlock()
			return res, nil

		case setProps:
			keys := make([]string, 0, len(p.props))
			for k := range p.props {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			body.WriteString(renderProps(p.props, keys))
			hasWrite = true

		case contentBlock:
			line, err := renderBlock(p, r)
			if err != nil {
				return res, err
			}
			body.WriteString(line)
			hosted = append(hosted, p.anchors...)
			hasWrite = true
		}
	}

	if !hasWrite {
		return res, nil
	}
	if tabID == "" {
		return res, fmt.Errorf("gdocs: no tab for %s; a create must precede its content", rel)
	}
	if err := b.replaceTabContent(ctx, docID, tabID, body.String()); err != nil {
		return res, err
	}

	// Every anchor this tab hosts resolves to the tab itself. That is deliberately
	// coarse: a real anchor resolves to a heading id, which is read-only and needs
	// the second read-then-write pass (#152), and that lands with the real
	// tokenizer (#160). Reporting the tab keeps every reference RESOLVABLE in the
	// meantime rather than failing the transport's readiness gate.
	for _, a := range hosted {
		res.Anchors[a] = publish.BackendID(tabID)
	}
	return res, nil
}

// createTab adds a tab and returns its server-minted id.
//
// parentTabId is deliberately never set: tabs are flat (#155). The parent Ref the
// optimizer stamped on this unit is still consumed by the transport's readiness
// gate, so ordering is unaffected.
func (b *Backend) createTab(ctx context.Context, docID, rel string) (string, error) {
	replies, err := b.c.batchUpdate(ctx, docID, []map[string]any{
		{"addDocumentTab": map[string]any{
			"tabProperties": map[string]any{"title": rel},
		}},
	})
	if err != nil {
		return "", fmt.Errorf("gdocs: create tab for %s: %w", rel, err)
	}
	id := tabIDFromReply(replies)
	if id == "" {
		return "", fmt.Errorf("gdocs: create tab for %s: reply carried no tabId", rel)
	}
	b.mu.Lock()
	b.tabs[rel] = id
	b.mu.Unlock()
	return id, nil
}

// deleteTab removes a tab. It CASCADES to child tabs, which is safe here only
// because tabs are flat.
func (b *Backend) deleteTab(ctx context.Context, docID, tabID string) error {
	_, err := b.c.batchUpdate(ctx, docID, []map[string]any{
		{"deleteTab": map[string]any{"tabId": tabID}},
	})
	return err
}

// replaceTabContent empties a tab and writes text into it.
//
// Both requests carry an explicit tabId. That is not optional politeness: an
// omitted tabId silently targets the FIRST tab of the document (#147), so a
// missing one here would quietly overwrite the wrong page.
func (b *Backend) replaceTabContent(ctx context.Context, docID, tabID, text string) error {
	end, err := b.tabEndIndex(ctx, docID, tabID)
	if err != nil {
		return err
	}
	var requests []map[string]any
	// A body's final newline cannot be deleted, so the range stops one short of it.
	if end > 2 {
		requests = append(requests, map[string]any{
			"deleteContentRange": map[string]any{
				"range": map[string]any{"tabId": tabID, "startIndex": 1, "endIndex": end - 1},
			},
		})
	}
	requests = append(requests, map[string]any{
		"insertText": map[string]any{
			"endOfSegmentLocation": map[string]any{"tabId": tabID},
			"text":                 text,
		},
	})
	_, err = b.c.batchUpdate(ctx, docID, requests)
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

// renderBlock renders one block to plain text, resolving every late-bound Ref.
//
// Resolution happens even though the rendering is plain: a Ref that cannot
// resolve is a contract violation the transport's gate should have prevented, and
// surfacing it here rather than silently dropping the reference is what keeps
// #160's real link rendering an addition rather than a repair.
func renderBlock(p contentBlock, r backend.Resolver) (string, error) {
	resolved, err := backend.ResolveRuns(p.runs, r)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, rr := range resolved {
		switch {
		case rr.Run.Ref != "":
			sb.WriteString(rr.Run.Text)
		case rr.Run.Link != "":
			sb.WriteString(rr.Run.Text)
		default:
			sb.WriteString(rr.Run.Text)
		}
	}
	line := strings.TrimRight(sb.String(), "\n")
	if line == "" {
		return "", nil
	}
	return line + "\n", nil
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
