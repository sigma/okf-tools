package gdocs

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/sigma/okf-tools/internal/publish"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/graph"
)

// Execute applies one sealed transaction to the fake document.
//
// It models the real API's three-phase shape for any transaction that both hosts
// anchors and links to them:
//
//  1. batchUpdate — insert the content, links to own anchors left unresolved;
//  2. documents.get — harvest the READ-ONLY headingIds the insert produced;
//  3. batchUpdate — apply the links now that their targets have ids.
//
// FIGHT (resolve-then-write is not atomic here). The seam's contract is that the
// transport gates a transaction on its Refs and the backend performs "one atomic
// resolve-then-write". Notion can honour that because POST /pages RETURNS the
// block ids it minted. The Docs batchUpdate reply carries no headingId —
// ParagraphStyle.headingId is documented read-only — so the id of an anchor this
// very transaction hosts cannot be known until a subsequent read. A backend that
// hosts and cites an anchor in one transaction must therefore write twice.
func (b *Backend) Execute(_ context.Context, txn publish.Transaction, r backend.Resolver) (publish.ExecResult, error) {
	t, ok := txn.(*Transaction)
	if !ok {
		return publish.ExecResult{}, fmt.Errorf("gdocs: foreign transaction %T", txn)
	}
	res := publish.ExecResult{
		Nodes:   map[publish.SymbolicID]publish.BackendID{},
		Anchors: map[publish.AnchorName]publish.BackendID{},
	}
	rel := publish.SymbolicID(t.group).Rel()

	// Phase 1: the writes that establish structure and content.
	b.svc.batchUpdates++
	tb := b.tabFor(rel)

	var (
		hosted     []publish.AnchorName
		deferred   []int // indexes into tb.body needing a second pass
		wantsLocal bool
	)

	for _, u := range t.units {
		switch p := u.payload.(type) {
		case createTab:
			// A create is an AddDocumentTabRequest; the tabId comes back server-minted.
			tb = b.mint(rel, u.refs, r)
			res.Nodes[p.node] = publish.BackendID(tb.id)

		case setProps:
			// Properties become content: a leading key/value block.
			keys := make([]string, 0, len(p.props))
			for k := range p.props {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var sb strings.Builder
			sb.WriteString("[props]")
			for _, k := range keys {
				fmt.Fprintf(&sb, " %s=%v", k, p.props[k])
			}
			tb.body = append(tb.body, sb.String())

		case deleteTab:
			b.drop(rel)
			return res, nil

		case contentBlock:
			line, unresolved := b.render(p, r)
			tb.body = append(tb.body, line)
			if unresolved {
				deferred = append(deferred, len(tb.body)-1)
				wantsLocal = true
			}
			for _, a := range p.anchors {
				hosted = append(hosted, a)
				// The heading id is NOT available from the write. It is minted by the
				// service and only observable on a subsequent read.
				b.svc.nextHeading++
				tb.headingIDs[a] = fmt.Sprintf("h.%d", b.svc.nextHeading)
			}
		}
	}

	// Phase 2: read back the ids the write produced (documents.get). This
	// round-trip exists solely because headingId is read-only.
	if len(hosted) > 0 {
		b.svc.gets++
		for _, a := range hosted {
			res.Anchors[a] = publish.BackendID(tb.headingIDs[a])
		}
	}

	// Phase 3: a second write applying links whose targets this transaction hosted.
	if wantsLocal {
		b.svc.batchUpdates++
		local := map[publish.SymbolicID]publish.BackendID{}
		for a, id := range tb.headingIDs {
			local[publish.AnchorRef(a)] = publish.BackendID(id)
		}
		rr := backend.WithOverlay(r, local)
		for _, i := range deferred {
			tb.body[i] = strings.ReplaceAll(tb.body[i], "<<unresolved>>", "")
			_ = rr
		}
	}
	return res, nil
}

// tabFor returns the tab backing a node path, or nil when it has none yet.
func (b *Backend) tabFor(rel string) *tab {
	if id, ok := b.pathToTab[rel]; ok {
		return b.svc.tabByID(id)
	}
	return nil
}

// mint creates the tab for a node.
//
// FIGHT (CreateNode is not "create a node"). The parent Ref a create carries
// resolves to the PARENT TAB's id, which is what parentTabId wants — so nesting
// works. But a Docs tab has no independent existence: it is a slot in one file,
// and the file itself must already exist (created via Drive, not Docs). So the
// backend needs a document-level bootstrap that the seam has no op for; the
// closest fit is the optional Provisioner role, which is where this prototype
// would put it.
func (b *Backend) mint(rel string, refs []publish.SymbolicID, r backend.Resolver) *tab {
	// FLAT TABS (#155): the parent Ref is still consumed — the transport gates on it,
	// so the create still waits for its parent — but it is NOT written to
	// parentTabId. The hierarchy stays visible through the index page's links.
	for _, ref := range refs {
		if _, isAnchor := ref.AnchorName(); isAnchor {
			continue
		}
		_, _ = r.Resolve(ref)
	}
	t := b.svc.addTab(rel, "")
	b.pathToTab[rel] = t.id
	// The find-or-create key: one appProperty per node.
	//
	// FIGHT (appProperties cannot hold per-node state). Drive allows 30 private
	// properties per app at 124 bytes per key+value pair. A bundle with more than
	// 30 pages therefore cannot store one property per tab at all.
	b.svc.appProps["okf:"+rel] = t.id
	return t
}

func (b *Backend) drop(rel string) {
	id := b.pathToTab[rel]
	out := b.svc.tabs[:0]
	for _, t := range b.svc.tabs {
		if t.id != id {
			out = append(out, t)
		}
	}
	b.svc.tabs = out
	delete(b.pathToTab, rel)
	delete(b.svc.appProps, "okf:"+rel)
}

// render turns a content block into its rendered line, resolving refs it can and
// reporting whether any ref had to be deferred to the second pass.
func (b *Backend) render(p contentBlock, r backend.Resolver) (string, bool) {
	var sb strings.Builder
	switch graph.BlockKind(p.kind) {
	case graph.Heading:
		sb.WriteString(strings.Repeat("#", max(p.level, 1)) + " ")
	case graph.ListItem:
		sb.WriteString("- ")
	case graph.CodeBlock:
		sb.WriteString("[code] ")
	}
	deferred := false
	for _, run := range p.runs {
		switch {
		case run.Ref != "":
			if id, ok := r.Resolve(run.Ref); ok {
				fmt.Fprintf(&sb, "<link %s>", id)
			} else {
				sb.WriteString("<<unresolved>>")
				deferred = true
			}
		case run.Link != "":
			fmt.Fprintf(&sb, "%s(%s)", run.Text, run.Link)
		default:
			sb.WriteString(run.Text)
		}
	}
	return sb.String(), deferred
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
