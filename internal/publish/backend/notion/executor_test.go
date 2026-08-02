package notion

import (
	"context"
	"net/http/httptest"
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
	base := []Option{WithBaseURL(srv.URL), WithToken("tok"), WithDataSourceID("ds1")}
	return New(append(base, opts...)...)
}

// paraBlock is a paragraph child block with literal text.
func paraBlock(text string) childBlock {
	return childBlock{kind: int(graph.Paragraph), runs: []textRun{{Text: text}}}
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
			runs: []textRun{{Text: "see "}, {Ref: "node:b.md"}},
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
			runs:     []textRun{{Text: "foo: bar"}},
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
		Children: []childBlock{{kind: int(graph.CodeBlock), runs: []textRun{{Text: "x"}}}},
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

// TestExecuteUnresolvedRefErrors: a content Ref the Resolver cannot resolve is a
// hard error (the transport must gate on it before Execute).
func TestExecuteUnresolvedRefErrors(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f)

	txn := &Transaction{
		Group: "node:a.md", Node: "node:a.md", Create: true,
		Children: []childBlock{{kind: int(graph.Paragraph), runs: []textRun{{Ref: "node:missing.md"}}}},
	}
	if _, err := be.Execute(context.Background(), txn, stubResolver{}); err == nil {
		t.Fatal("expected an error for an unresolved content Ref")
	}
}
