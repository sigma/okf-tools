package notion

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/graph"
)

// agreeResolver resolves every ref to a stable fake id — enough for
// blockContentJSON to render a page mention, whose text the live side folds to the
// ref placeholder anyway.
type agreeResolver struct{}

func (agreeResolver) Resolve(id publish.SymbolicID) (publish.BackendID, bool) {
	return publish.BackendID("id-" + string(id)), true
}

// loadAgreementBundle materializes a bundle with one content-rich doc — heading,
// paragraph carrying a cross-ref, list, code fence, quote, and a GFM table — so the
// agreement proof runs over every block shape the projection must reconcile.
func loadAgreementBundle(t *testing.T) *bundle.Bundle {
	t.Helper()
	files := map[string]string{
		"okf.toml": "[glossary]\nenabled = true\nfiles = [\"CONTEXT.md\"]\n",
		"index.md": "---\nokf_version: \"0.1\"\n---\n# Root\n",
		"docs/adr/rich.md": "---\ntype: adr\ntitle: Rich\n---\n" +
			"# Heading One\n\n" +
			"## Heading Two\n\n" +
			"###### Heading Six\n\n" + // clamps to heading_3 on both sides
			"A paragraph that links to [B](b.md) inline.\n\n" +
			"- first item\n- second item\n\n" +
			"```go\nfn main() {}\n```\n\n" +
			"> a quoted line\n\n" +
			"| A | B |\n| - | - |\n| c1 | [B](b.md) |\n", // a ref inside a table cell
		"docs/adr/b.md": "---\ntype: adr\ntitle: B\n---\nBody of B.\n",
		"CONTEXT.md":    "# Glossary\n\n**Root KEK**: the root key-encryption key.\n",
	}
	return loadTestBundle(t, files)
}

func docByRel(t *testing.T, b *bundle.Bundle, rel string) *bundle.Doc {
	t.Helper()
	for _, d := range b.Docs {
		if d.Rel == rel {
			return d
		}
	}
	t.Fatalf("doc %q not in bundle", rel)
	return nil
}

// parseRaw marshals a raw live-block map and runs it back through the real
// parseLiveBlock, so the live side of the proof exercises production decoding.
func parseRaw(t *testing.T, m map[string]any) liveBlock {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	lb, err := parseLiveBlock(raw)
	if err != nil {
		t.Fatalf("parseLiveBlock: %v", err)
	}
	return lb
}

// annotateRuns stamps the server-filled plain_text Notion returns on read but the
// write payload omits, so the live half exercises the production runText branch
// that PREFERS plain_text. A page mention gets a title plain_text to prove the
// scanner discards it (folding to the ref placeholder) rather than hashing it.
func annotateRuns(runs []map[string]any) {
	for _, r := range runs {
		switch r["type"] {
		case "text":
			if txt, ok := r["text"].(map[string]any); ok {
				r["plain_text"] = txt["content"]
			}
		case nTypeMention:
			r["plain_text"] = "Mention Title — must be discarded"
		}
	}
}

// liveBlocksFromUnits renders tokenized source units to the exact Notion block
// JSON the Executor writes (then stamped with server-filled plain_text), and parses
// them back as the recompute scanner would — a table's rows ride as their own
// table_row blocks. This is the live half of the round-trip: what a real GET
// /blocks/{id}/children would serve after this publish.
func liveBlocksFromUnits(t *testing.T, units []publish.AtomicUnit) []liveBlock {
	t.Helper()
	var out []liveBlock
	for i, u := range units {
		cb, ok := u.Payload.(childBlock)
		if !ok {
			t.Fatalf("unit %d payload is %T, want childBlock", i, u.Payload)
		}
		typ, payload, err := blockContentJSON(cb, agreeResolver{})
		if err != nil {
			t.Fatalf("blockContentJSON: %v", err)
		}
		if runs, ok := payload["rich_text"].([]map[string]any); ok {
			annotateRuns(runs)
		}
		out = append(out, parseRaw(t, map[string]any{
			"object": "block", "id": fmt.Sprintf("blk%d", i), "type": typ, typ: payload,
		}))
		if typ != nTypeTable {
			continue
		}
		children, _ := payload["children"].([]map[string]any)
		for j, ch := range children {
			if tr, ok := ch[nTypeTableRow].(map[string]any); ok {
				if cells, ok := tr["cells"].([]any); ok {
					for _, cell := range cells {
						if cellRuns, ok := cell.([]map[string]any); ok {
							annotateRuns(cellRuns)
						}
					}
				}
			}
			if _, has := ch["id"]; !has {
				ch["id"] = fmt.Sprintf("blk%d-row%d", i, j)
			}
			out = append(out, parseRaw(t, ch))
		}
	}
	return out
}

// TestRecomputeHashSourceLiveAgreement is the keystone of #110: the source-side
// content hash and the live-scan content hash of the SAME published page are
// byte-identical. If they diverge, recompute re-clobbers the page every run; this
// pins them together across every block shape — heading (clamped h1/h2/h6),
// paragraph, list, code, quote, a table (including a ref in a cell), and a
// cross-ref — both with and without the block-0 banner (whose quote+link is the
// one shape the link-fold logic exists for).
func TestRecomputeHashSourceLiveAgreement(t *testing.T) {
	b := loadAgreementBundle(t)
	be := newServer(t, newFakeNotion())
	d := docByRel(t, b, "docs/adr/rich.md")

	banner := &graph.Banner{Text: "Generated page — edit at source.", BaseURL: "https://github.com/sigma/ideas", Ref: "main"}
	for _, tc := range []struct {
		name   string
		banner *graph.Banner
	}{
		{"no banner", nil},
		{"with banner", banner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := be.sourceContentHash(d, tc.banner)
			live := liveContentHash(liveBlocksFromUnits(t, be.Tokenize(graph.ProjectDocument(d, tc.banner))))
			if source != live {
				t.Fatalf("source and live content hashes disagree:\n source = %s\n live   = %s", source, live)
			}
		})
	}
}

// TestCanonRunsTextSourceLiveAgreement pins the two mirror serializers directly,
// bypassing the round-trip so it catches divergence the end-to-end proof can't:
// plain_text is PREFERRED over content on the live side, a mention folds to the ref
// placeholder discarding its server-filled title, and an external link URL is
// folded on both sides. The source and live extractions of the same run sequence
// must match exactly.
func TestCanonRunsTextSourceLiveAgreement(t *testing.T) {
	var live []annotatedRun
	if err := json.Unmarshal([]byte(`[
		{"type":"text","plain_text":"see ","text":{"content":"see "}},
		{"type":"mention","plain_text":"Target Page Title","mention":{"type":"page","page":{"id":"pg"}}},
		{"type":"text","plain_text":" and edit","text":{"content":" and edit","link":{"url":"https://example.com/edit"}}}
	]`), &live); err != nil {
		t.Fatal(err)
	}
	source := []publish.Run{
		{Text: "see "},
		{Ref: "node:target.md"},
		{Text: " and edit", Link: "https://example.com/edit"},
	}

	got, want := liveCanonRunsText(live), canonRunsText(source)
	if got != want {
		t.Fatalf("mirror serializers disagree:\n live   = %q\n source = %q", got, want)
	}
	// And the value is what we expect: placeholder for the mention, link folded in.
	expect := "see " + canonRefPlaceholder + " and edit" + canonLinkSep + "https://example.com/edit"
	if got != expect {
		t.Fatalf("canonical text = %q, want %q", got, expect)
	}
}

// TestRecomputeHashDetectsBodyDrift guards the other direction: the aligned hash
// still moves when live content actually drifts, so a real edit is not skipped.
func TestRecomputeHashDetectsBodyDrift(t *testing.T) {
	b := loadAgreementBundle(t)
	be := newServer(t, newFakeNotion())
	d := docByRel(t, b, "docs/adr/rich.md")

	clean := liveBlocksFromUnits(t, be.Tokenize(graph.ProjectDocument(d, nil)))
	drifted := append([]liveBlock(nil), clean...)
	drifted[0].text += " (hand-edited in Notion)"

	if liveContentHash(clean) == liveContentHash(drifted) {
		t.Fatal("a live body edit must change the content hash")
	}
}
