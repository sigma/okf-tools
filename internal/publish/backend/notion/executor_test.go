package notion

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/graph"
)

// stubResolver is a fixed resolution table for Executor tests in isolation from the
// transport.
type stubResolver map[publish.SymbolicID]publish.BackendID

func (s stubResolver) Resolve(id publish.SymbolicID) (publish.BackendID, bool) {
	b, ok := s[id]
	return b, ok
}

// newServer wires a fake Notion server to a backend aimed at it, offline.
func newServer(t *testing.T, f *fakeNotion, opts ...Option) *Backend {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	// Pacing off: the offline tests exercise request *content*, and the global
	// interval would charge each of them wall-clock delay per request.
	base := []Option{WithBaseURL(srv.URL), WithToken("tok"), WithDataSourceID("ds1"), WithInterval(0)}
	return New(append(base, opts...)...)
}

// paraBlock is a paragraph child block with literal text.
func paraBlock(text string) childBlock {
	return childBlock{kind: int(graph.Paragraph), runs: []publish.Run{{Text: text}}}
}

// digInto walks a nested map[string]any by keys, failing on a missing/typed step.
func digInto(t *testing.T, m map[string]any, keys ...string) map[string]any {
	t.Helper()
	cur := m
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			t.Fatalf("expected map at %q, got %T in %v", k, cur[k], cur)
		}
		cur = next
	}
	return cur
}

// TestExecuteCreatePostsFusedPage: a create transaction becomes a POST /pages
// parenting under the data source, carrying its properties and children, and its
// minted page id is returned in ExecResult.Nodes.
func TestExecuteCreatePostsFusedPage(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:a.md", Node: "node:a.md", Create: true,
		Props:    map[string]any{"title": "A"},
		Children: []childBlock{paraBlock("hello"), paraBlock("world")},
	}
	res, err := be.Execute(context.Background(), txn, stubResolver{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	posts := f.requestsTo("POST", "/pages")
	if len(posts) != 1 {
		t.Fatalf("want 1 POST /pages, got %d", len(posts))
	}
	parent := digInto(t, posts[0].Body, "parent")
	if parent["data_source_id"] != "ds1" {
		t.Errorf("top-level create should parent under the data source, parent = %v", parent)
	}
	if children, _ := posts[0].Body["children"].([]any); len(children) != 2 {
		t.Errorf("want 2 children in the POST body, got %v", posts[0].Body["children"])
	}
	title := digInto(t, posts[0].Body, "properties", "title")
	if _, ok := title["title"]; !ok {
		t.Errorf("title should serialize as a Notion title property, got %v", title)
	}

	id, ok := res.Nodes["node:a.md"]
	if !ok || id == "" {
		t.Errorf("ExecResult should map node:a.md to its minted id, got %v", res.Nodes)
	}
}

// TestExecuteResolvesContentRef proves the physical Ref→BackendID swap happens
// behind the seam: an inline content Ref is serialized as a mention carrying the
// resolved backend id, using the Resolver the transport passes in.
func TestExecuteResolvesContentRef(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:a.md", Node: "node:a.md", Create: true,
		Children: []childBlock{{
			kind: int(graph.Paragraph),
			runs: []publish.Run{{Text: "see "}, {Ref: "node:b.md"}},
		}},
	}
	r := stubResolver{"node:b.md": "page-b-real"}
	if _, err := be.Execute(context.Background(), txn, r); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	body := f.requestsTo("POST", "/pages")[0].Body
	children, _ := body["children"].([]any)
	block, _ := children[0].(map[string]any)
	para := digInto(t, block, "paragraph")
	rich, _ := para["rich_text"].([]any)
	// runs: [text "see ", mention → resolved id]
	mention, _ := rich[1].(map[string]any)
	page := digInto(t, mention, "mention", "page")
	if page["id"] != "page-b-real" {
		t.Errorf("content Ref should be swapped for the resolved backend id, got %v", page["id"])
	}
}

// TestExecuteCreateResolvesParent: a subpage create resolves its parent Ref to the
// real parent page id.
func TestExecuteCreateResolvesParent(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:child.md", Node: "node:child.md", Create: true,
		Parent: "node:index.md",
	}
	r := stubResolver{"node:index.md": "page-index-real"}
	if _, err := be.Execute(context.Background(), txn, r); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	parent := digInto(t, f.requestsTo("POST", "/pages")[0].Body, "parent")
	if parent["page_id"] != "page-index-real" {
		t.Errorf("subpage should parent under the resolved parent page, got %v", parent)
	}
}

// TestExecuteSubpageCreateSendsTitleOnly: a cluster subpage nests as a child_page
// under its cluster-index page, so its parent is a page, not the data source. A
// page-parented child_page has only a title — none of the data-source column
// properties. The create must therefore drop the column props for a subpage, or
// Notion 400s "Invalid property identifier" (sigma/okf-tools#104). A top-level,
// data-source-parented create keeps the full property set.
func TestExecuteSubpageCreateSendsTitleOnly(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:child.md", Node: "node:child.md", Create: true,
		Parent: "node:index.md",
		Props: map[string]any{
			"title": "Child", "type": "note", "status": "draft", "created": "2026-01-01",
		},
	}
	r := stubResolver{"node:index.md": "page-index-real"}
	if _, err := be.Execute(context.Background(), txn, r); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	props := digInto(t, f.requestsTo("POST", "/pages")[0].Body, "properties")
	if _, ok := props["title"]; !ok {
		t.Errorf("subpage create should carry the title property, got %v", props)
	}
	// The brief's contract is that a subpage carries *only* title — no
	// data-source columns leak through, so title must be the sole key.
	if len(props) != 1 {
		t.Errorf("subpage create must carry only the title property, got %v", props)
	}
}

// TestExecuteCodeBlockCarriesLanguage: a code child block serializes with the
// Notion-required `language` field, mapped from the fence token, alongside its
// rich text. Without this, Notion rejects the create with a 400 validation_error.
func TestExecuteCodeBlockCarriesLanguage(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:a.md", Node: "node:a.md", Create: true,
		Children: []childBlock{{
			kind:     int(graph.CodeBlock),
			language: "yml", // alias for yaml
			runs:     []publish.Run{{Text: "foo: bar"}},
		}},
	}
	if _, err := be.Execute(context.Background(), txn, stubResolver{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	body := f.requestsTo("POST", "/pages")[0].Body
	children, _ := body["children"].([]any)
	block, _ := children[0].(map[string]any)
	code := digInto(t, block, "code")
	if code["language"] != "yaml" {
		t.Errorf("code block should carry a mapped language, got %v", code["language"])
	}
	if _, ok := code["rich_text"]; !ok {
		t.Errorf("code block should still carry rich_text, got %v", code)
	}
}

// TestExecuteCodeBlockDefaultsLanguage: a code block whose fence named no (or an
// unknown) language defaults to Notion's "plain text", never an empty/undefined
// value that Notion rejects.
func TestExecuteCodeBlockDefaultsLanguage(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:a.md", Node: "node:a.md", Create: true,
		Children: []childBlock{{kind: int(graph.CodeBlock), runs: []publish.Run{{Text: "x"}}}},
	}
	if _, err := be.Execute(context.Background(), txn, stubResolver{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	body := f.requestsTo("POST", "/pages")[0].Body
	children, _ := body["children"].([]any)
	block, _ := children[0].(map[string]any)
	code := digInto(t, block, "code")
	if code["language"] != "plain text" {
		t.Errorf("code block with no fence language should default to plain text, got %v", code["language"])
	}
}

// TestExecuteParagraphHasNoLanguage: only code blocks gain a language key; every
// other block type keeps its uniform `{type: {rich_text}}` shape.
func TestExecuteParagraphHasNoLanguage(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:a.md", Node: "node:a.md", Create: true,
		Children: []childBlock{paraBlock("hi")},
	}
	if _, err := be.Execute(context.Background(), txn, stubResolver{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	body := f.requestsTo("POST", "/pages")[0].Body
	children, _ := body["children"].([]any)
	block, _ := children[0].(map[string]any)
	para := digInto(t, block, "paragraph")
	if _, ok := para["language"]; ok {
		t.Errorf("paragraph should not carry a language key, got %v", para)
	}
}

// TestExecuteBannerRunCarriesLink: a run with an external Link (the disclaimer
// banner's source deep-link) serializes as a Notion text object carrying
// text.link.url — a plain hyperlink, not a page mention.
func TestExecuteBannerRunCarriesLink(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	const url = "https://github.com/sigma/ideas/edit/main/docs/adr/0015.md"
	txn := &Transaction{
		Group: "node:a.md", Node: "node:a.md", Create: true,
		Children: []childBlock{{
			kind: int(graph.Quote),
			runs: []publish.Run{{Text: "Generated from the repo", Link: url}},
		}},
	}
	if _, err := be.Execute(context.Background(), txn, stubResolver{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	body := f.requestsTo("POST", "/pages")[0].Body
	children, _ := body["children"].([]any)
	block, _ := children[0].(map[string]any)
	if block["type"] != "quote" {
		t.Fatalf("banner block type = %v, want quote", block["type"])
	}
	quote := digInto(t, block, "quote")
	rich, _ := quote["rich_text"].([]any)
	if len(rich) != 1 {
		t.Fatalf("want one rich_text span, got %d: %v", len(rich), rich)
	}
	span, _ := rich[0].(map[string]any)
	text := digInto(t, span, "text")
	if text["content"] != "Generated from the repo" {
		t.Errorf("content = %v", text["content"])
	}
	link, ok := text["link"].(map[string]any)
	if !ok {
		t.Fatalf("banner run should carry a text.link object, got %v", text)
	}
	if link["url"] != url {
		t.Errorf("link url = %v, want %v", link["url"], url)
	}
}

// TestExecutePlainTextRunHasNoLink: a run without a Link keeps the bare text
// object — no empty link key that Notion would reject.
func TestExecutePlainTextRunHasNoLink(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:a.md", Node: "node:a.md", Create: true,
		Children: []childBlock{paraBlock("hello")},
	}
	if _, err := be.Execute(context.Background(), txn, stubResolver{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	body := f.requestsTo("POST", "/pages")[0].Body
	children, _ := body["children"].([]any)
	block, _ := children[0].(map[string]any)
	para := digInto(t, block, "paragraph")
	rich, _ := para["rich_text"].([]any)
	span, _ := rich[0].(map[string]any)
	text := digInto(t, span, "text")
	if _, ok := text["link"]; ok {
		t.Errorf("plain run should carry no link key, got %v", text)
	}
}

// TestExecuteCreateMapsAnchors: a create whose child hosts an anchor lists the
// created children and maps the anchor to that child's block id.
func TestExecuteCreateMapsAnchors(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	hosting := paraBlock("Root KEK")
	hosting.anchors = []publish.AnchorName{"glossary/root-kek"}
	txn := &Transaction{
		Group: "node:CONTEXT.md", Node: "node:CONTEXT.md", Create: true,
		Children: []childBlock{hosting},
	}
	res, err := be.Execute(context.Background(), txn, stubResolver{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if f.countPath("GET", "/blocks/page-1/children") != 1 {
		t.Errorf("a create hosting an anchor should list children to learn block ids")
	}
	if got, ok := res.Anchors["glossary/root-kek"]; !ok || got == "" {
		t.Errorf("anchor should resolve to the hosting child's block id, got %v", res.Anchors)
	}
}

// TestExecuteCreateResolvesSelfHostedAnchorOnFreshCreate reproduces #89: a
// glossary host that both defines an anchor and cites its own anchor must publish
// on a genuinely fresh create — an empty seed with no scan-seeded anchor. The
// citing block's self-hosted ref cannot resolve before the POST mints the anchor
// block id, so the create must defer it out of the POST and patch it in once the
// id exists, rather than fail with "content ref … did not resolve".
func TestExecuteCreateResolvesSelfHostedAnchorOnFreshCreate(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	hosting := paraBlock("Emergency block: a fast, time-boxed suspension")
	hosting.anchors = []publish.AnchorName{"glossary/emergency-block"}
	citing := childBlock{
		kind: int(graph.Paragraph),
		runs: []publish.Run{{Text: "held while an "}, {Ref: "anchor:glossary/emergency-block"}},
	}
	txn := &Transaction{
		Group: "node:CONTEXT.md", Node: "node:CONTEXT.md", Create: true,
		Children: []childBlock{hosting, citing},
	}

	// Empty seed: the empty-mirror first publish, with no scan-seeded anchor id.
	res, err := be.Execute(context.Background(), txn, stubResolver{})
	if err != nil {
		t.Fatalf("Execute: a fresh create with a self-hosted anchor should succeed, got %v", err)
	}

	// The anchor maps to its hosting child's block id (block-2: page-1 is the page,
	// block-2 the first child, block-3 the second).
	anchorID, ok := res.Anchors["glossary/emergency-block"]
	if !ok || anchorID == "" {
		t.Fatalf("anchor should resolve to its hosting block id, got %v", res.Anchors)
	}

	// The POST defers the self-hosted citation: the citing block carries no mention.
	post := f.requestsTo("POST", "/pages")[0].Body
	children, _ := post["children"].([]any)
	if len(children) != 2 {
		t.Fatalf("want 2 children posted (blocks preserved one-for-one), got %v", post["children"])
	}
	citedInPost := digInto(t, children[1].(map[string]any), "paragraph")
	for _, run := range asSlice(citedInPost["rich_text"]) {
		if m, _ := run.(map[string]any); m["type"] == "mention" {
			t.Errorf("the POST must defer the self-hosted citation, not send an unresolved mention: %v", m)
		}
	}

	// The deferred citation is patched in once the anchor block id exists, carrying
	// the real anchor block id as the mention target. block-3 is the citing child
	// (page-1 the page, block-2 the hosting child, block-3 the citing child).
	citingID := "block-3"
	blockPatches := f.requestsTo("PATCH", "/blocks/"+citingID)
	if len(blockPatches) != 1 {
		t.Fatalf("want 1 in-place patch of the citing block %s, got %d (reqs: %v)", citingID, len(blockPatches), f.reqs)
	}
	patched := digInto(t, blockPatches[0].Body, "paragraph")
	var mentionID any
	for _, run := range asSlice(patched["rich_text"]) {
		if m, _ := run.(map[string]any); m["type"] == "mention" {
			mentionID = digInto(t, m, "mention", "page")["id"]
		}
	}
	if mentionID != string(anchorID) {
		t.Errorf("patched citation should mention the anchor's block id %q, got %v", anchorID, mentionID)
	}
}

// asSlice coerces a decoded JSON value to a slice, tolerating a nil/absent value.
func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// TestExecuteScanSeededAnchorCiteNeedsNoDeferral guards the #89 "host already
// exists" path (no regression to the scan-seeded resolution). When the host page
// exists, a re-publish citing its anchor is a content append (a non-create
// transaction), and the scan has seeded the anchor id into the resolver — the case
// that already resolved. The append must carry the seeded anchor mention and must
// NOT trigger the create-only deferral (no in-place block patch).
func TestExecuteScanSeededAnchorCiteNeedsNoDeferral(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:CONTEXT.md", Node: "node:CONTEXT.md",
		Children: []childBlock{{
			kind: int(graph.Paragraph),
			runs: []publish.Run{{Text: "held while an "}, {Ref: "anchor:glossary/emergency-block"}},
		}},
	}
	r := stubResolver{
		"node:CONTEXT.md":                 "page-ctx-real",
		"anchor:glossary/emergency-block": "block-anchor-real",
	}
	if _, err := be.Execute(context.Background(), txn, r); err != nil {
		t.Fatalf("Execute: a scan-seeded self-hosted citation should resolve, got %v", err)
	}

	if f.countPath("POST", "/pages") != 0 {
		t.Errorf("an append against an existing host must not create a page")
	}
	appends := f.requestsTo("PATCH", "/blocks/page-ctx-real/children")
	if len(appends) != 1 {
		t.Fatalf("want 1 content append to the existing host, got %d", len(appends))
	}
	// No in-place block content patch: the create-only deferral path must not fire.
	for _, req := range f.reqs {
		if req.Method == "PATCH" && strings.HasPrefix(req.Path, "/blocks/") && !strings.HasSuffix(req.Path, "/children") {
			t.Errorf("scan-seeded path must not issue an in-place block patch, got %s", req.Path)
		}
	}
	// The append resolved the citation through the scan seed.
	block, _ := asSlice(appends[0].Body["children"])[0].(map[string]any)
	para := digInto(t, block, "paragraph")
	var mentionID any
	for _, run := range asSlice(para["rich_text"]) {
		if m, _ := run.(map[string]any); m["type"] == "mention" {
			mentionID = digInto(t, m, "mention", "page")["id"]
		}
	}
	if mentionID != "block-anchor-real" {
		t.Errorf("append should mention the scan-seeded anchor id, got %v", mentionID)
	}
}

// TestExecuteAppendResolvesSelfHostedAnchor reproduces #102: #89's self-hosted
// anchor fix covered only the create path, but cluster subpage nesting can force the
// optimizer to split a glossary host's create away from its content, so the block
// that both hosts and cites its own anchor rides a non-create *append* transaction.
// On a fresh publish the anchor is not scan-seeded (contrast
// TestExecuteScanSeededAnchorCiteNeedsNoDeferral) — it is hosted *in this append* —
// so the append, like the create, must defer the self-citation out of the append,
// learn the appended block ids, and patch the citation back in, rather than fail
// with "content ref … did not resolve".
func TestExecuteAppendResolvesSelfHostedAnchor(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	hosting := paraBlock("Emergency block: a fast, time-boxed suspension")
	hosting.anchors = []publish.AnchorName{"glossary/emergency-block"}
	citing := childBlock{
		kind: int(graph.Paragraph),
		runs: []publish.Run{{Text: "held while an "}, {Ref: "anchor:glossary/emergency-block"}},
	}
	// A non-create (append) transaction: the page already exists (its create ran in an
	// earlier txn from the force-split), but the anchor is hosted in this append, not
	// scan-seeded — so the resolver resolves the page and nothing else.
	txn := &Transaction{
		Group: "node:CONTEXT.md", Node: "node:CONTEXT.md",
		Children: []childBlock{hosting, citing},
	}
	r := stubResolver{"node:CONTEXT.md": "page-ctx-real"}

	res, err := be.Execute(context.Background(), txn, r)
	if err != nil {
		t.Fatalf("Execute: a fresh append hosting+citing its own anchor should succeed, got %v", err)
	}

	if f.countPath("POST", "/pages") != 0 {
		t.Errorf("an append must not create a page")
	}
	// The anchor maps to its hosting child's appended block id (block-1: the first
	// appended child, block-2 the second — no page is minted on an append).
	anchorID, ok := res.Anchors["glossary/emergency-block"]
	if !ok || anchorID == "" {
		t.Fatalf("the self-hosted anchor never resolved, anchors = %v", res.Anchors)
	}

	// The append defers the self-hosted citation: the citing block carries no mention.
	appends := f.requestsTo("PATCH", "/blocks/page-ctx-real/children")
	if len(appends) != 1 {
		t.Fatalf("want 1 content append to the existing host, got %d", len(appends))
	}
	children, _ := appends[0].Body["children"].([]any)
	if len(children) != 2 {
		t.Fatalf("want 2 children appended (blocks preserved one-for-one), got %v", children)
	}
	citedInAppend := digInto(t, children[1].(map[string]any), "paragraph")
	for _, run := range asSlice(citedInAppend["rich_text"]) {
		if m, _ := run.(map[string]any); m["type"] == "mention" {
			t.Errorf("the append must defer the self-hosted citation, not send an unresolved mention: %v", m)
		}
	}

	// The deferred citation is patched in once the appended block ids exist, carrying
	// the real anchor block id as the mention target. block-2 is the citing child
	// (block-1 the hosting child).
	citingID := "block-2"
	blockPatches := f.requestsTo("PATCH", "/blocks/"+citingID)
	if len(blockPatches) != 1 {
		t.Fatalf("want 1 in-place patch of the citing block %s, got %d (reqs: %v)", citingID, len(blockPatches), f.reqs)
	}
	patched := digInto(t, blockPatches[0].Body, "paragraph")
	var mentionID any
	for _, run := range asSlice(patched["rich_text"]) {
		if m, _ := run.(map[string]any); m["type"] == "mention" {
			mentionID = digInto(t, m, "mention", "page")["id"]
		}
	}
	if mentionID != string(anchorID) {
		t.Errorf("patched citation should mention the anchor's block id %q, got %v", anchorID, mentionID)
	}
}

// TestExecuteAppendsOverflow: a non-create content transaction appends its children
// to the resolved write-target page via PATCH /blocks/{id}/children.
func TestExecuteAppendsOverflow(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{Group: "node:a.md", Children: []childBlock{paraBlock("tail")}}
	r := stubResolver{"node:a.md": "page-a-real"}
	if _, err := be.Execute(context.Background(), txn, r); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if f.countPath("PATCH", "/blocks/page-a-real/children") != 1 {
		t.Errorf("overflow should append to the resolved page, requests = %v", f.reqs)
	}
	if f.countPath("POST", "/pages") != 0 {
		t.Errorf("an overflow append must not create a page")
	}
}

// TestExecuteArchivesDelete: a delete transaction archives the resolved page.
func TestExecuteArchivesDelete(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{Group: "node:old.md", Node: "node:old.md", Delete: true}
	r := stubResolver{"node:old.md": "page-old-real"}
	if _, err := be.Execute(context.Background(), txn, r); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	patches := f.requestsTo("PATCH", "/pages/page-old-real")
	if len(patches) != 1 {
		t.Fatalf("want 1 archive PATCH, got %d", len(patches))
	}
	if patches[0].Body["archived"] != true {
		t.Errorf("delete should archive the page, body = %v", patches[0].Body)
	}
}

// TestExecutePropsOnlyUpdate: a standalone SetProperties transaction patches the
// resolved page's properties and writes no children.
func TestExecutePropsOnlyUpdate(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{Group: "node:a.md", Node: "node:a.md", Props: map[string]any{"title": "A2"}}
	r := stubResolver{"node:a.md": "page-a-real"}
	if _, err := be.Execute(context.Background(), txn, r); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if f.countPath("PATCH", "/pages/page-a-real") != 1 {
		t.Errorf("props-only should PATCH the page, requests = %v", f.reqs)
	}
	if f.countPath("PATCH", "/blocks/page-a-real/children") != 0 {
		t.Errorf("props-only should not append children")
	}
}

// TestExecuteSubpagePropsUpdateSendsTitleOnly: the #104 parent-kind rule holds on
// the UPDATE path too, not just the create (sigma/okf-tools#128). A standalone
// SetProperties against a page-parented node — the shape a cluster subpage reaches
// on every run whose scan seeded no property hash for it — must PATCH only the
// title; sending the data-source columns 400s "Invalid property identifier".
func TestExecuteSubpagePropsUpdateSendsTitleOnly(t *testing.T) {
	f := newFakeNotion()
	f.childPages["page-child-real"] = true
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:child.md", Node: "node:child.md",
		Parent: "node:index.md",
		Props: map[string]any{
			"title": "Child", "type": "note", "status": "draft", "created": "2026-01-01",
		},
	}
	r := stubResolver{"node:child.md": "page-child-real", "node:index.md": "page-index-real"}
	if _, err := be.Execute(context.Background(), txn, r); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	reqs := f.requestsTo("PATCH", "/pages/page-child-real")
	if len(reqs) != 1 {
		t.Fatalf("want 1 property PATCH, got %d", len(reqs))
	}
	props := digInto(t, reqs[0].Body, "properties")
	if _, ok := props["title"]; !ok {
		t.Errorf("subpage update should carry the title property, got %v", props)
	}
	if len(props) != 1 {
		t.Errorf("subpage update must carry only the title property, got %v", props)
	}
}

// TestExecuteTopLevelPropsUpdateSendsFullProps: the other side of the parent-kind
// rule — a data-source-parented node has a row of its own, so a standalone
// SetProperties against it still PATCHes the full column set.
func TestExecuteTopLevelPropsUpdateSendsFullProps(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:a.md", Node: "node:a.md",
		Props: map[string]any{"title": "A", "type": "note", "status": "draft"},
	}
	r := stubResolver{"node:a.md": "page-a-real"}
	if _, err := be.Execute(context.Background(), txn, r); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	reqs := f.requestsTo("PATCH", "/pages/page-a-real")
	if len(reqs) != 1 {
		t.Fatalf("want 1 property PATCH, got %d", len(reqs))
	}
	props := digInto(t, reqs[0].Body, "properties")
	for _, k := range []string{"title", "type", "status"} {
		if _, ok := props[k]; !ok {
			t.Errorf("top-level update should carry %q, got %v", k, props)
		}
	}
}

// TestExecuteUnresolvedRefErrors: a content Ref the Resolver cannot resolve is a
// hard error (the transport must gate on it before Execute).
func TestExecuteUnresolvedRefErrors(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:a.md", Node: "node:a.md", Create: true,
		Children: []childBlock{{kind: int(graph.Paragraph), runs: []publish.Run{{Ref: "node:missing.md"}}}},
	}
	if _, err := be.Execute(context.Background(), txn, stubResolver{}); err == nil {
		t.Fatal("expected an error for an unresolved content Ref")
	}
}

// --- identity at create time (#135) -----------------------------------------

// A top-level row carries its identifying `path` from the moment it exists, not
// from write-back: an interrupted run must leave an identifiable row rather than an
// anonymous one no later run can match to a source node or reclaim.
func TestCreateWritesThePathColumn(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:docs/adr/0002.md", Node: "node:docs/adr/0002.md", Create: true,
		Props: map[string]any{"title": "ADR 2"},
	}
	if _, err := be.Execute(context.Background(), txn, stubResolver{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	props := digInto(t, f.requestsTo("POST", "/pages")[0].Body, "properties")
	got := plainTextOf(t, props["path"])
	if got != "docs/adr/0002.md" {
		t.Errorf("create wrote path %q, want the node's bundle-relative path", got)
	}
}

// The parent-kind rule still binds: a cluster subpage is a child_page with none of
// the data source's columns, so it must not be sent a path either (#104, #128).
func TestSubpageCreateOmitsThePathColumn(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:docs/cluster/child.md", Node: "node:docs/cluster/child.md", Create: true,
		Parent: "node:docs/cluster/index.md",
		Props:  map[string]any{"title": "Child"},
	}
	r := stubResolver{"node:docs/cluster/index.md": "page-index-real"}
	if _, err := be.Execute(context.Background(), txn, r); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	props := digInto(t, f.requestsTo("POST", "/pages")[0].Body, "properties")
	if _, ok := props["path"]; ok {
		t.Errorf("subpage create must not carry a path column, got %v", props)
	}
	if len(props) != 1 {
		t.Errorf("subpage create must carry only the title property, got %v", props)
	}
}

// plainTextOf reads the concatenated text out of a recorded rich_text property
// value, the shape the derived columns are written in.
func plainTextOf(t *testing.T, v any) string {
	t.Helper()
	prop, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("property is not an object: %#v", v)
	}
	spans, ok := prop["rich_text"].([]any)
	if !ok {
		t.Fatalf("property carries no rich_text: %#v", prop)
	}
	var out string
	for _, span := range spans {
		m, _ := span.(map[string]any)
		text, _ := m["text"].(map[string]any)
		s, _ := text["content"].(string)
		out += s
	}
	return out
}
