package notion

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

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
//
// A self-hosted anchor (a child that cites an anchor this same transaction hosts)
// cannot resolve inside the POST: Notion mints the anchor's block id only once the
// page is created. The optimizer suppresses such a ref from the transport gate,
// assuming the backend resolves it intra-object — but this backend resolves refs
// itself before the POST, so the id is not yet known (sigma/okf-tools#89). So the
// citation is deferred out of the POST and patched back in once the block id
// exists, the notion counterpart of the #76 fs fix (where ids are deterministic
// rather than server-minted).
func (b *Backend) create(ctx context.Context, t *Transaction, r backend.Resolver, res *publish.ExecResult) error {
	parent, err := b.parentOf(t, r)
	if err != nil {
		return err
	}
	hosted := hostedAnchorNames(t.Children)
	postChildren, deferred := deferSelfHostedCites(t.Children, hosted)
	children, err := b.childrenJSON(postChildren, r)
	if err != nil {
		return err
	}

	var page object
	req := createPageReq{Parent: parent, Properties: b.createProps(t), Children: children}
	if err := b.do(ctx, http.MethodPost, "/pages", req, &page); err != nil {
		return err
	}
	res.Nodes[writeTarget(t)] = publish.BackendID(page.ID)

	if hosted == nil {
		return nil
	}
	// The POST /pages does not echo the created child ids, so GET them to learn the
	// server-minted block ids before mapping anchors and patching deferred citations.
	ids, err := b.listChildIDs(ctx, page.ID)
	if err != nil {
		return err
	}
	return b.resolveSelfHostedAnchors(ctx, fmt.Sprintf("notion: create %s", writeTarget(t)), t.Children, ids, deferred, res.Anchors, r)
}

// createProps builds the property set a page-create sends: the transaction's own
// properties through the parent-kind seam, plus — for a top-level row — the `path`
// column that identifies which source node the row IS.
//
// Writing path here rather than only at write-back is what makes an interrupted run
// recoverable (sigma/okf-tools#135). Both scan modes key a row to its node by its
// path and skip a row without one as "not ours", so a row created by a run that died
// before write-back was invisible to every later run: not matched to its source node
// (which was therefore created again) and not recognisable as a leak. A row is now
// identifiable from the moment it exists.
//
// A page-parented node is a cluster subpage — a child_page carrying no data-source
// columns — so it gets no path, for the same reason it gets no other column (#104,
// #128). Its identity lives in its parent row's subtree map instead.
func (b *Backend) createProps(t *Transaction) map[string]any {
	props := b.pageProps(t.Parent, t.Props)
	if t.Parent == "" {
		props["path"] = b.richTextProp(writeTarget(t).Rel())
	}
	return props
}

// patchSelfHostedCites re-materializes the self-hosted anchor citations the POST
// deferred (sigma/okf-tools#89). With the hosting blocks created and their anchor
// block ids now known, it re-renders each deferred child in full — the self-hosted
// refs resolvable through an overlay layered on the transport Resolver — and
// PATCHes it in place, so the self-referencing content lands with real mentions.
// deferred holds the indices into children (and, positionally, into ids) whose
// citations were stripped from the POST; it is empty for the common create, which
// pays nothing.
func (b *Backend) patchSelfHostedCites(ctx context.Context, children []childBlock, ids []string, deferred []int, anchors map[publish.AnchorName]publish.BackendID, base backend.Resolver) error {
	if len(deferred) == 0 {
		return nil
	}
	local := make(map[publish.SymbolicID]publish.BackendID, len(anchors))
	for name, bid := range anchors {
		local[publish.AnchorRef(name)] = bid
	}
	r := backend.WithOverlay(base, local)
	for _, i := range deferred {
		typ, payload, err := blockContentJSON(children[i], r)
		if err != nil {
			return err
		}
		path := "/blocks/" + url.PathEscape(ids[i])
		if err := b.do(ctx, http.MethodPatch, path, map[string]any{typ: payload}, nil); err != nil {
			return err
		}
	}
	return nil
}

// update executes a non-create, non-delete transaction against an existing page:
// a property PATCH (a standalone SetProperties, or a fused properties-without-
// create write) and/or a children write. The page id is resolved from the
// transaction's write-target Ref, which the transport gated on.
//
// The children write comes in two shapes, told apart by AssertsContent. A content
// assertion writes the node's COMPLETE expected content, so it clears the page's
// existing children before appending — otherwise a re-publish appends a second copy
// of the body below the first, and compounds on every run (sigma/okf-tools#130). A
// continuation is a follow-on overflow chunk of an assertion an earlier transaction
// of the same node already made; it only appends, or a large page would keep just
// its last chunk. The transport guarantees that ordering: it holds a group to its
// packing order, so the assertion always precedes its continuations.
//
// A self-hosted anchor can ride the append too, not just the create (sigma/okf-
// tools#102): when cluster subpage nesting force-splits a glossary host's create
// away from its content, the block that both hosts and cites its own anchor lands in
// this append. On a fresh publish that anchor is not scan-seeded, and — as in the
// create path (#89) — its Notion block id is minted only by the append. So the
// append mirrors the create's two-phase write: defer the self-hosted citation out of
// the append, learn the appended block ids, then patch the citation back in. (The
// scan-seeded re-publish, where the anchor id is already resolved and hosted by no
// child of this append, defers nothing and patches nothing — see the #89 tests.)
func (b *Backend) update(ctx context.Context, t *Transaction, r backend.Resolver, res *publish.ExecResult) error {
	target := writeTarget(t)
	id, ok := r.Resolve(target)
	if !ok {
		return fmt.Errorf("notion: update: write-target %s did not resolve", target)
	}

	if t.Props != nil {
		// Through the same parent-aware seam the create uses: a page-parented node is
		// a child_page and takes only its title, or Notion 400s (#104, #128). An empty
		// set (a titleless subpage) is nothing to write, so skip the call entirely.
		if props := b.pageProps(t.Parent, t.Props); len(props) > 0 {
			req := updatePageReq{Properties: props}
			if err := b.do(ctx, http.MethodPatch, "/pages/"+url.PathEscape(string(id)), req, nil); err != nil {
				return err
			}
		}
	}

	if len(t.Children) > 0 {
		if t.AssertsContent {
			if err := b.clearChildren(ctx, string(id)); err != nil {
				return fmt.Errorf("notion: replace content of %s: %w", target, err)
			}
		}
		hosted := hostedAnchorNames(t.Children)
		appendChildren, deferred := deferSelfHostedCites(t.Children, hosted)
		children, err := b.childrenJSON(appendChildren, r)
		if err != nil {
			return err
		}
		var out appendResult
		path := "/blocks/" + url.PathEscape(string(id)) + "/children"
		if err := b.do(ctx, http.MethodPatch, path, appendChildrenReq{Children: children}, &out); err != nil {
			return err
		}
		if hosted != nil {
			// The append echoes the minted block ids directly (unlike the create's
			// POST /pages, which needs a follow-up GET), so map anchors against them.
			if err := b.resolveSelfHostedAnchors(ctx, fmt.Sprintf("notion: append to %s", target), t.Children, objectIDs(out.Results), deferred, res.Anchors, r); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveSelfHostedAnchors completes the two-phase self-hosted-anchor write once the
// hosting blocks exist and their ids are known (positionally aligned with children):
// it records each hosted anchor's block id, then re-materializes the citations
// deferred out of the write. The create and append paths share this tail and differ
// only in how they learn ids — the create GETs the page's children, the append reads
// them from its response (sigma/okf-tools#89, #102). errPrefix labels the wrapped
// error with the operation and its write-target.
func (b *Backend) resolveSelfHostedAnchors(ctx context.Context, errPrefix string, children []childBlock, ids []string, deferred []int, anchors map[publish.AnchorName]publish.BackendID, r backend.Resolver) error {
	if err := mapAnchors(children, ids, anchors); err != nil {
		return fmt.Errorf("%s: %w", errPrefix, err)
	}
	if err := b.patchSelfHostedCites(ctx, children, ids, deferred, anchors, r); err != nil {
		return fmt.Errorf("%s: %w", errPrefix, err)
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

// clearChildren empties a page of the blocks that are its own content — the first
// half of a content assertion's clear-then-append (sigma/okf-tools#130). It lists
// the page's children through the same paginated helper the create path's anchor
// mapping uses, then archives each with DELETE /blocks/{id}.
//
// A `child_page` child is skipped: it is not this page's content but its own node
// (the rule recompute_hash.go states when it excludes a child_page from a page's
// hash). A cluster index page holds its subpages as child_page blocks, so deleting
// them would archive whole pages the assertion never claimed to own — and the
// parent row's `hashes` subtree would still record them, so no later run would
// notice or restore them.
//
// Two consequences are deliberate, not oversights:
//
//   - It destroys edits made by hand in Notion. Replacement IS the contract of
//     SetContent: the mirror is a function of the source, and a page whose body can
//     drift from it makes the stored content hash meaningless as a change signal.
//     Published pages carry a generated-page banner saying so.
//   - It is not atomic. An abort between the deletes and the append leaves the page
//     truncated. That is accepted because it self-corrects: a failed run never
//     reaches write-back, so the node's content hash is never stamped, so the next
//     run cannot conclude "unchanged" and re-asserts the full body.
//
// It costs one GET plus one DELETE per existing block, all through the global pacer
// — the price of a content change roughly doubles. Notion offers no bulk block
// delete, so there is no cheaper way to assert a page's whole body.
func (b *Backend) clearChildren(ctx context.Context, pageID string) error {
	children, err := b.listChildren(ctx, pageID)
	if err != nil {
		return err
	}
	for _, c := range children {
		if c.Type == nTypeChildPage {
			continue
		}
		if err := b.do(ctx, http.MethodDelete, "/blocks/"+url.PathEscape(c.ID), nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// listChildIDs returns a page's child block ids in order — the create path's way to
// learn the ids POST /pages does not echo, so hosted anchors can be mapped.
func (b *Backend) listChildIDs(ctx context.Context, pageID string) ([]string, error) {
	children, err := b.listChildren(ctx, pageID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(children))
	for i, c := range children {
		ids[i] = c.ID
	}
	return ids, nil
}

// listChildren pages through GET /blocks/{id}/children, returning the child blocks
// in order with their ids and Notion types. The type is what lets clearChildren
// tell a page's own content from a child_page that is a node in its own right;
// callers that only match positionally against what they just sent take the ids
// alone through listChildIDs.
func (b *Backend) listChildren(ctx context.Context, pageID string) ([]object, error) {
	var out []object
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
		out = append(out, page.Results...)
		if !page.HasMore || page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
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

// blockJSON serializes one Notion child block for a POST /pages / append children
// array: the {object, type, <type>:payload} envelope wrapping its type-keyed
// rich-text payload, with inline Refs resolved to page mentions.
func blockJSON(cb childBlock, r backend.Resolver) (map[string]any, error) {
	typ, payload, err := blockContentJSON(cb, r)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"object": "block",
		"type":   typ,
		typ:      payload,
	}, nil
}

// blockContentJSON serializes one child block's Notion type and its type-keyed
// content payload (the {rich_text: …}, plus a code block's required language),
// resolving inline Refs to page mentions. blockJSON wraps it in the child-array
// envelope; the self-hosted-anchor patch (#89) sends the bare {type: payload} to
// PATCH /blocks/{id} to re-materialize a deferred citation in place.
func blockContentJSON(cb childBlock, r backend.Resolver) (string, map[string]any, error) {
	if cb.kind == int(graph.Table) {
		return tableBlockJSON(cb, r)
	}
	rich, err := richTextJSON(cb.runs, r)
	if err != nil {
		return "", nil, err
	}
	typ := notionBlockType(cb.kind, cb.level)
	payload := map[string]any{"rich_text": rich}
	// A Notion `code` block requires a `language`; every other block type the
	// uniform builder emits has no such required field. Map the fence token to
	// Notion's enum, defaulting empty/unknown to "plain text". Key off the neutral
	// kind rather than the serialized type string so this stays one branch.
	if cb.kind == int(graph.CodeBlock) {
		payload["language"] = notionCodeLanguage(cb.language)
	}
	return typ, payload, nil
}

// tableBlockJSON serializes a table childBlock into Notion's nested `table` payload:
// a table_width / has_column_header / has_row_header header plus the rows as child
// `table_row` blocks, each carrying its cells as arrays of resolved rich text. The
// width is the header row's cell count (rows were normalized rectangular upstream),
// and has_row_header is always false — GFM has no row-header notion. Every cell's
// inline Refs resolve through the Resolver exactly as a paragraph's runs do.
func tableBlockJSON(cb childBlock, r backend.Resolver) (string, map[string]any, error) {
	width := 0
	if len(cb.rows) > 0 {
		width = len(cb.rows[0].cells)
	}
	children := make([]map[string]any, 0, len(cb.rows))
	for _, row := range cb.rows {
		cells := make([]any, 0, len(row.cells))
		for _, cellRuns := range row.cells {
			rich, err := richTextJSON(cellRuns, r)
			if err != nil {
				return "", nil, err
			}
			cells = append(cells, rich)
		}
		children = append(children, map[string]any{
			"object":    "block",
			"type":      "table_row",
			"table_row": map[string]any{"cells": cells},
		})
	}
	payload := map[string]any{
		"table_width":       width,
		"has_column_header": cb.hasColumnHeader,
		"has_row_header":    false,
		"children":          children,
	}
	return "table", payload, nil
}

// richTextJSON turns a block's inline runs into Notion rich-text objects: a literal
// span becomes a text object; a late-bound Ref is resolved through the Resolver —
// the physical Ref→BackendID swap — into a page mention carrying the real id. An
// unresolved Ref is an error, since the transport must gate on it before Execute.
func richTextJSON(runs []publish.Run, r backend.Resolver) ([]map[string]any, error) {
	resolved, err := backend.ResolveRuns(runs, r)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(resolved))
	for _, rr := range resolved {
		run := rr.Run
		if run.Ref != "" {
			out = append(out, map[string]any{
				"type": "mention",
				"mention": map[string]any{
					"type": "page",
					"page": map[string]any{"id": string(rr.RefID)},
				},
			})
			continue
		}
		if run.Text == "" {
			continue
		}
		txt := map[string]any{"content": run.Text}
		if run.Link != "" {
			// An external hyperlink: Notion carries it as the text object's link.url
			// (the banner's source deep-link). A mention would need a page id; this is
			// a plain web URL, so it rides as a link, not a mention.
			txt["link"] = map[string]any{"url": run.Link}
		}
		out = append(out, map[string]any{
			"type": "text",
			"text": txt,
		})
	}
	return out, nil
}

// propsJSON reshapes the neutral property map into Notion page properties, keying
// each value's Notion shape off its column's declared kind in schema.json: a
// select column becomes {select:{name}}, a list a multi_select, a date a
// {date:{start}}, a number/checkbox their scalar, and text (or any unmapped key) a
// rich_text. The title column stays a title value. Without a schema the split
// falls back to the legacy behavior — the literal "title" key becomes a title,
// everything else rich_text. The derived self-describing columns
// (path/hash/hashes/anchors) are written by the #44 write-back, not here.
//
// With a schema present, a key that is neither the pipeline-derived title/type nor
// a declared source:frontmatter column is silently dropped (issue #86): the mirror
// is provisioned only from the schema, so writing an undeclared key 400s as "not a
// property that exists". This lets reserved pages (README/section pages) carry
// extra frontmatter — e.g. a cluster README's author — and still publish cleanly.
func (b *Backend) propsJSON(props map[string]any) map[string]any {
	out := make(map[string]any, len(props))
	for k, v := range props {
		if !b.writableColumn(k) {
			continue
		}
		out[k] = b.propertyValue(k, v)
	}
	return out
}

// pageProps builds the Notion property set a transaction may write to its node, for
// the node's parent kind. It is the ONE place the parent-kind rule lives: every
// write path — the create's POST /pages and the update's PATCH /pages alike — routes
// its property set through here, so a third write path inherits the rule instead of
// re-discovering the bug (sigma/okf-tools#128).
//
// A data-source row (top-level node) carries the full pipeline-derived property set.
// A page-parented node is a cluster subpage — a child_page under its cluster index —
// which has only a title and none of the data source's column properties (`created`,
// `status`, `type`, …); sending them 400s as "Invalid property identifier"
// (sigma/okf-tools#104). This mirrors the scan model: a subpage has no row of its own
// (its {id, hash} folds into the parent row's hashes subtree), so it is not a row and
// must not carry column properties.
//
// A subpage is assumed to carry a pipeline-derived title (every real cluster subpage
// does); with none, the write sends empty properties rather than fabricating a title
// — Notion rejects a titleless child_page either way, and the update path skips the
// PATCH entirely.
func (b *Backend) pageProps(parent publish.SymbolicID, props map[string]any) map[string]any {
	full := b.propsJSON(props)
	if parent == "" {
		return full
	}
	if title, ok := full["title"]; ok {
		return map[string]any{"title": title}
	}
	return map[string]any{}
}

// writableColumn reports whether a neutral property key may be written as a page
// property. Without a schema every key is writable (legacy behavior). With one,
// only the pipeline-derived title/type and the schema's declared source:frontmatter
// columns are writable; any other key (undeclared frontmatter, or a page's extra
// keys) is dropped so it never reaches a mirror that has no such column.
func (b *Backend) writableColumn(name string) bool {
	if b.schema == nil {
		return true
	}
	if name == "title" || name == "type" {
		return true
	}
	col, ok := b.schema.Lookup(name)
	return ok && col.IsFrontmatter()
}

// propertyValue serializes one neutral property into its Notion value object. It
// looks the key up in the schema to find the column's kind; a key the schema does
// not declare (or a run with no schema at all) falls back to the legacy split:
// the literal "title" key is a title, everything else rich_text.
func (b *Backend) propertyValue(name string, v any) map[string]any {
	kind := ""
	if b.schema != nil {
		if col, ok := b.schema.Lookup(name); ok {
			kind = col.Kind
		}
	}
	if kind == "" {
		if name == "title" {
			kind = "title"
		} else {
			kind = "text"
		}
	}

	switch kind {
	case "title":
		return map[string]any{"title": b.richTextSpans(fmt.Sprint(v))}
	case "select":
		s := fmt.Sprint(v)
		if s == "" {
			return map[string]any{"select": nil}
		}
		return map[string]any{"select": map[string]any{"name": s}}
	case "list":
		return map[string]any{"multi_select": multiSelectNames(v)}
	case "date":
		start, ok := notionDate(v)
		if !ok {
			return map[string]any{"date": nil}
		}
		return map[string]any{"date": map[string]any{"start": start}}
	case "number":
		return map[string]any{"number": notionNumber(v)}
	case "checkbox":
		return map[string]any{"checkbox": notionBool(v)}
	default: // "text" and any unknown kind
		return map[string]any{"rich_text": b.richTextSpans(fmt.Sprint(v))}
	}
}

// richTextSpans wraps s in the rich-text array a title or rich_text property value
// carries, splitting it into as many spans as the per-span char cap requires so no
// single span exceeds Notion's 2000-char limit — the same cap splitRuns applies to
// a block's inline runs (a single oversized span 400s, #94). The spans concatenate
// on read (plainText), so round-trip reads are unaffected.
func (b *Backend) richTextSpans(s string) []map[string]any {
	chunks := splitByChars(s, b.maxChars)
	spans := make([]map[string]any, len(chunks))
	for i, c := range chunks {
		spans[i] = map[string]any{"type": "text", "text": map[string]any{"content": c}}
	}
	return spans
}

// multiSelectNames turns a neutral list value into a Notion multi_select value —
// one {name} object per element. YAML decodes a frontmatter list to []any; a lone
// scalar is treated as a single-element list. Empty elements are dropped, so an
// absent-but-present tags key yields an empty (cleared) multi_select rather than a
// blank option.
func multiSelectNames(v any) []map[string]any {
	var names []string
	switch xs := v.(type) {
	case []any:
		for _, x := range xs {
			if s := fmt.Sprint(x); s != "" {
				names = append(names, s)
			}
		}
	case []string:
		for _, s := range xs {
			if s != "" {
				names = append(names, s)
			}
		}
	case nil:
		// no elements
	default:
		if s := fmt.Sprint(v); s != "" {
			names = append(names, s)
		}
	}
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"name": n})
	}
	return out
}

// notionDate normalizes a neutral date value into a Notion date `start` string.
// yaml.v3 decodes an unquoted ISO date (created: 2026-07-18) into a time.Time and
// a quoted one into a string; both reduce to an ISO-8601 start Notion accepts
// (date-only when there is no time-of-day). An empty/absent value yields ok=false
// so the caller clears the column instead of writing a bogus start.
func notionDate(v any) (string, bool) {
	switch d := v.(type) {
	case time.Time:
		if d.Hour() == 0 && d.Minute() == 0 && d.Second() == 0 && d.Nanosecond() == 0 {
			return d.Format("2006-01-02"), true
		}
		return d.Format(time.RFC3339), true
	case string:
		if d == "" {
			return "", false
		}
		return d, true
	case nil:
		return "", false
	default:
		s := fmt.Sprint(v)
		if s == "" {
			return "", false
		}
		return s, true
	}
}

// notionNumber coerces a neutral number value into the JSON number Notion's number
// property carries. A numeric-looking string is parsed; anything non-numeric (or
// absent) yields nil, which clears the column.
func notionNumber(v any) any {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return n
	case float64:
		return n
	case string:
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return f
		}
		return nil
	default:
		return nil
	}
}

// notionBool coerces a neutral checkbox value into a bool. A YAML bool passes
// through; a string is truthy only for the conventional affirmatives.
func notionBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		switch b {
		case "true", "yes", "1":
			return true
		default:
			return false
		}
	default:
		return false
	}
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
	case graph.Table:
		return "table"
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

// deferSelfHostedCites splits a create's children for the two-phase self-hosted
// anchor write (sigma/okf-tools#89). It returns the children to POST — a copy in
// which every run citing an anchor this same transaction hosts is stripped, since
// that anchor's Notion block id does not exist until after the POST — together
// with the indices of the blocks whose citations were deferred, to be patched back
// in once the ids are known. Blocks are preserved one-for-one (only offending runs
// are dropped) so the created children stay positionally aligned for anchor
// mapping. hosted is the transaction's own hosted-anchor set (nil when it hosts
// none); when no child cites one of those anchors it returns the original slice and
// no deferrals, so the common create is untouched and pays no copy.
func deferSelfHostedCites(children []childBlock, hosted map[publish.AnchorName]bool) ([]childBlock, []int) {
	if hosted == nil {
		return children, nil
	}
	var (
		out      []childBlock
		deferred []int
	)
	for i, cb := range children {
		stripped, changed := dropCites(cb, hosted)
		if changed {
			if out == nil {
				out = make([]childBlock, len(children))
				copy(out, children)
			}
			out[i] = stripped
			deferred = append(deferred, i)
		}
	}
	if out == nil {
		return children, nil
	}
	return out, deferred
}

// hostedAnchorNames collects the set of anchor names the transaction's own children
// host, or nil when it hosts none.
func hostedAnchorNames(children []childBlock) map[publish.AnchorName]bool {
	var hosted map[publish.AnchorName]bool
	for _, cb := range children {
		for _, a := range cb.anchors {
			if hosted == nil {
				hosted = map[publish.AnchorName]bool{}
			}
			hosted[a] = true
		}
	}
	return hosted
}

// dropCites returns cb with every run citing a self-hosted anchor removed, and
// whether any run was dropped. The block's identity (kind, level, hosted anchors)
// is untouched — only the deferred citation runs go, to be patched back once the
// anchor block ids exist.
func dropCites(cb childBlock, hosted map[publish.AnchorName]bool) (childBlock, bool) {
	changed := false
	runs := make([]publish.Run, 0, len(cb.runs))
	for _, run := range cb.runs {
		if name, ok := run.Ref.AnchorName(); ok && hosted[name] {
			changed = true
			continue
		}
		runs = append(runs, run)
	}
	if !changed {
		return cb, false
	}
	cb.runs = runs
	return cb, true
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
