package notion

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/graph"
)

// Execute serializes one sealed Notion Transaction into its wire call(s),
// substituting every late-bound Ref through the transport's Resolver as it writes,
// and returns the resolution-table updates the call produced.
//
// The three transaction shapes map to three write paths: a Create is a POST /pages
// fusing properties and the first content chunk; a Delete is a PATCH /pages
// archive; anything else is an update — a property PATCH and/or a PATCH children
// append. The physical Ref→BackendID swap lives entirely here, behind the seam:
// the transport gates a transaction on its Refs being resolved and hands in the
// Resolver, and this method resolves the parent, the write-target page, and every
// inline content Ref as it builds the payload. The ExecResult reports the created
// node's new backend id and each hosted anchor's block id.
func (b *Backend) Execute(ctx context.Context, txn publish.Transaction, r backend.Resolver) (publish.ExecResult, error) {
	t, ok := txn.(*Transaction)
	if !ok {
		return publish.ExecResult{}, fmt.Errorf("notion: Execute got a foreign transaction type %T", txn)
	}

	res := publish.ExecResult{
		Nodes:   map[publish.SymbolicID]publish.BackendID{},
		Anchors: map[publish.AnchorName]publish.BackendID{},
	}

	switch {
	case t.Delete:
		return res, b.archive(ctx, t, r)
	case t.Create:
		return res, b.create(ctx, t, r, &res)
	default:
		return res, b.update(ctx, t, r, &res)
	}
}

// create executes a POST /pages: it parents the page (a data-source row for a
// top-level node, or the resolved parent page for a subpage), fuses the properties
// and the first content chunk, records the minted page id, and — if any child
// hosts an anchor — lists the created children to map anchor-name → block id.
func (b *Backend) create(ctx context.Context, t *Transaction, r backend.Resolver, res *publish.ExecResult) error {
	parent, err := b.parentOf(t, r)
	if err != nil {
		return err
	}
	children, err := b.childrenJSON(t.Children, r)
	if err != nil {
		return err
	}

	var page object
	req := createPageReq{Parent: parent, Properties: propsJSON(t.Props), Children: children}
	if err := b.do(ctx, http.MethodPost, "/pages", req, &page); err != nil {
		return err
	}
	res.Nodes[writeTarget(t)] = publish.BackendID(page.ID)

	if hostsAnchors(t.Children) {
		ids, err := b.listChildIDs(ctx, page.ID)
		if err != nil {
			return err
		}
		if err := mapAnchors(t.Children, ids, res.Anchors); err != nil {
			return fmt.Errorf("notion: create %s: %w", writeTarget(t), err)
		}
	}
	return nil
}

// update executes a non-create, non-delete transaction against an existing page:
// a property PATCH (a standalone SetProperties, or a fused properties-without-
// create write) and/or a PATCH children append (content overflow). The page id is
// resolved from the transaction's write-target Ref, which the transport gated on.
func (b *Backend) update(ctx context.Context, t *Transaction, r backend.Resolver, res *publish.ExecResult) error {
	target := writeTarget(t)
	id, ok := r.Resolve(target)
	if !ok {
		return fmt.Errorf("notion: update: write-target %s did not resolve", target)
	}

	if t.Props != nil {
		req := updatePageReq{Properties: propsJSON(t.Props)}
		if err := b.do(ctx, http.MethodPatch, "/pages/"+url.PathEscape(string(id)), req, nil); err != nil {
			return err
		}
	}

	if len(t.Children) > 0 {
		children, err := b.childrenJSON(t.Children, r)
		if err != nil {
			return err
		}
		var out appendResult
		path := "/blocks/" + url.PathEscape(string(id)) + "/children"
		if err := b.do(ctx, http.MethodPatch, path, appendChildrenReq{Children: children}, &out); err != nil {
			return err
		}
		if hostsAnchors(t.Children) {
			if err := mapAnchors(t.Children, objectIDs(out.Results), res.Anchors); err != nil {
				return fmt.Errorf("notion: append to %s: %w", target, err)
			}
		}
	}
	return nil
}

// archive executes a DeleteNode as a PATCH /pages archive of the orphan's page,
// whose id resolves from the scan seed (a DeleteNode produces nothing, so its
// write-target Ref is scan-seeded and gated by the transport).
func (b *Backend) archive(ctx context.Context, t *Transaction, r backend.Resolver) error {
	target := writeTarget(t)
	id, ok := r.Resolve(target)
	if !ok {
		return fmt.Errorf("notion: archive: node %s did not resolve", target)
	}
	archived := true
	return b.do(ctx, http.MethodPatch, "/pages/"+url.PathEscape(string(id)), updatePageReq{Archived: &archived}, nil)
}

// parentOf builds the POST /pages parent: the data source for a top-level node
// (empty Parent), or the resolved parent page for a subpage.
func (b *Backend) parentOf(t *Transaction, r backend.Resolver) (pageParent, error) {
	if t.Parent == "" {
		return pageParent{Type: "data_source_id", DataSourceID: b.dataSourceID}, nil
	}
	id, ok := r.Resolve(t.Parent)
	if !ok {
		return pageParent{}, fmt.Errorf("notion: create %s: parent %s did not resolve", t.Node, t.Parent)
	}
	return pageParent{Type: "page_id", PageID: string(id)}, nil
}

// listChildIDs pages through GET /blocks/{id}/children, returning the child block
// ids in order — the create path's way to learn the ids POST /pages does not echo,
// so hosted anchors can be mapped.
func (b *Backend) listChildIDs(ctx context.Context, pageID string) ([]string, error) {
	var ids []string
	cursor := ""
	for {
		path := "/blocks/" + url.PathEscape(pageID) + "/children?page_size=100"
		if cursor != "" {
			path += "&start_cursor=" + url.QueryEscape(cursor)
		}
		var page childrenList
		if err := b.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		for _, o := range page.Results {
			ids = append(ids, o.ID)
		}
		if !page.HasMore || page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return ids, nil
}

// --- payload serialization --------------------------------------------------

// childrenJSON serializes a transaction's Notion child blocks, resolving every
// inline Ref as it goes.
func (b *Backend) childrenJSON(children []childBlock, r backend.Resolver) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(children))
	for _, cb := range children {
		blk, err := blockJSON(cb, r)
		if err != nil {
			return nil, err
		}
		out = append(out, blk)
	}
	return out, nil
}

// blockJSON serializes one Notion child block: its type-keyed rich-text payload,
// with inline Refs resolved to page mentions.
func blockJSON(cb childBlock, r backend.Resolver) (map[string]any, error) {
	rich, err := richTextJSON(cb.runs, r)
	if err != nil {
		return nil, err
	}
	typ := notionBlockType(cb.kind, cb.level)
	return map[string]any{
		"object": "block",
		"type":   typ,
		typ:      map[string]any{"rich_text": rich},
	}, nil
}

// richTextJSON turns a block's inline runs into Notion rich-text objects: a literal
// span becomes a text object; a late-bound Ref is resolved through the Resolver —
// the physical Ref→BackendID swap — into a page mention carrying the real id. An
// unresolved Ref is an error, since the transport must gate on it before Execute.
func richTextJSON(runs []textRun, r backend.Resolver) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		if run.Ref != "" {
			id, ok := r.Resolve(run.Ref)
			if !ok {
				return nil, fmt.Errorf("notion: content ref %s did not resolve", run.Ref)
			}
			out = append(out, map[string]any{
				"type": "mention",
				"mention": map[string]any{
					"type": "page",
					"page": map[string]any{"id": string(id)},
				},
			})
			continue
		}
		if run.Text == "" {
			continue
		}
		out = append(out, map[string]any{
			"type": "text",
			"text": map[string]any{"content": run.Text},
		})
	}
	return out, nil
}

// propsJSON reshapes the neutral property map into Notion page properties: the
// title column becomes a title value, every other key a rich_text value. The
// derived self-describing columns (path/hash/hashes/anchors) are written by the
// #44 write-back, not here.
func propsJSON(props map[string]any) map[string]any {
	out := make(map[string]any, len(props))
	for k, v := range props {
		span := []map[string]any{{"type": "text", "text": map[string]any{"content": fmt.Sprint(v)}}}
		if k == "title" {
			out[k] = map[string]any{"title": span}
		} else {
			out[k] = map[string]any{"rich_text": span}
		}
	}
	return out
}

// notionBlockType maps the neutral block kind (mirrored as an int on the child
// block) and level to a Notion block type.
func notionBlockType(kind, level int) string {
	switch graph.BlockKind(kind) {
	case graph.Heading:
		switch {
		case level <= 1:
			return "heading_1"
		case level == 2:
			return "heading_2"
		default:
			return "heading_3"
		}
	case graph.ListItem:
		return "bulleted_list_item"
	case graph.CodeBlock:
		return "code"
	case graph.Quote:
		return "quote"
	default:
		return "paragraph"
	}
}

// --- small helpers ----------------------------------------------------------

// writeTarget is the symbolic id of the page a transaction addresses. A create
// carries it as Node; every other shape addresses its group's node, and GroupKey
// is that node's symbolic id, so the group doubles as the write-target.
func writeTarget(t *Transaction) publish.SymbolicID {
	if t.Node != "" {
		return t.Node
	}
	return publish.SymbolicID(t.Group)
}

// hostsAnchors reports whether any child block hosts a named anchor.
func hostsAnchors(children []childBlock) bool {
	for _, cb := range children {
		if len(cb.anchors) > 0 {
			return true
		}
	}
	return false
}

// mapAnchors matches sent child blocks to their created block ids positionally
// (Notion returns children in submission order) and records each hosted anchor's
// block id. A length mismatch is an error rather than a silent misalignment.
func mapAnchors(children []childBlock, ids []string, dst map[publish.AnchorName]publish.BackendID) error {
	if len(ids) != len(children) {
		return fmt.Errorf("returned %d child ids for %d sent blocks, cannot map anchors", len(ids), len(children))
	}
	for i, cb := range children {
		for _, a := range cb.anchors {
			dst[a] = publish.BackendID(ids[i])
		}
	}
	return nil
}

// objectIDs projects the ids out of a slice of Notion objects.
func objectIDs(objs []object) []string {
	ids := make([]string, len(objs))
	for i, o := range objs {
		ids[i] = o.ID
	}
	return ids
}
