package graph

import (
	"context"
	"maps"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
)

// Option configures Generate.
type Option func(*options)

type options struct {
	hash func(*bundle.Doc) publish.Hash
}

// WithHasher overrides the expected-content hasher used for change detection, so
// a backend whose scanner reconstructs a matching hash can keep both sides in
// lockstep. The default is ContentHash.
func WithHasher(fn func(*bundle.Doc) publish.Hash) Option {
	return func(o *options) {
		if fn != nil {
			o.hash = fn
		}
	}
}

// Generate is Stage 1 of the okfpub pipeline: it builds the backend-neutral
// operation DAG (op-DAG) from a bundle and a current-state scan.
//
// Each source page is diffed independently by a concurrent worker into semantic
// ops carrying late-bound symbolic ids — with zero API-limit awareness — and the
// dependency edges are then assembled structurally (the three causes of #162:
// parent-before-child, content-refs-node, content-refs-anchor). A Ref whose
// target is present in the scan seed produces no edge, so an unchanged bundle
// yields a near-edgeless graph.
//
// Generate performs no backend calls; cs is treated purely as an input (nil is
// an empty snapshot). ctx only cancels the fan-out.
func Generate(ctx context.Context, b *bundle.Bundle, cs *publish.CurrentState, opts ...Option) (*Graph, error) {
	o := options{hash: ContentHash}
	for _, opt := range opts {
		opt(&o)
	}
	if cs == nil {
		cs = publish.NewCurrentState(nil, nil, nil)
	}

	// Concurrent per-page diff → ops. Results are slotted by index so op order is
	// deterministic (b.Docs is sorted by rel) regardless of goroutine scheduling.
	src := srcHierarchy(b.Docs)
	results := make([][]*Op, len(b.Docs))
	var wg sync.WaitGroup
	for i, d := range b.Docs {
		wg.Add(1)
		go func(i int, d *bundle.Doc) {
			defer wg.Done()
			if ctx.Err() != nil {
				return
			}
			results[i] = diffDoc(d, cs, &o, src)
		}(i, d)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	g := &Graph{}
	for _, ops := range results {
		g.Ops = append(g.Ops, ops...)
	}
	// Orphans: scanned nodes with no source → DeleteNode on the subtree roots.
	g.Ops = append(g.Ops, orphanOps(b, cs)...)

	g.Edges = assembleEdges(g.Ops)
	return g, nil
}

// diffDoc emits the ops for one source page from the uniform diff→ops mapping:
//
//	new              → CreateNode + SetProperties + SetContent
//	changed/drifted  → SetProperties + SetContent
//	unchanged        → nothing (hash-skip)
//
// (Vanished nodes have no source and are handled by orphanOps.)
func diffDoc(d *bundle.Doc, cs *publish.CurrentState, o *options, src *hierarchy) []*Op {
	node := nodeRef(d.Rel)
	parent := src.parent(d.Rel)
	hash := o.hash(d)
	title := d.Title()
	doc, refs, anchors := buildDocument(d)
	// Stamp the parent, expected hash, and title on the property/content ops (not
	// just the create) so publish-time write-back can route and record a touched
	// node whether it is new or a re-asserted existing one.
	setProps := &Op{Kind: SetProperties, Node: node, Props: propsOf(d), Parent: parent, Hash: hash, Title: title}
	setContent := &Op{Kind: SetContent, Node: node, Doc: doc, Refs: refs, Anchors: anchors, Parent: parent, Hash: hash, Title: title}

	if _, exists := cs.NodeID(node); !exists {
		return []*Op{{Kind: CreateNode, Node: node, Parent: parent, Hash: hash, Title: title}, setProps, setContent}
	}
	// Existing: hash-skip iff the scanned hash matches the expected one. A missing
	// scanned hash cannot confirm "unchanged", so it falls through to "changed".
	if got, ok := cs.ContentHash(node); ok && got == hash {
		return nil
	}
	return []*Op{setProps, setContent}
}

// propsOf builds the semantic property set SetProperties asserts: the doc's
// frontmatter columns plus its derived title (and type when present). Values are
// backend-neutral; the backend maps them to its own columns.
func propsOf(d *bundle.Doc) map[string]any {
	props := maps.Clone(d.Frontmatter)
	if props == nil {
		props = map[string]any{}
	}
	props["title"] = d.Title()
	if t := d.Type(); t != "" {
		props["type"] = t
	}
	return props
}

// hierarchy resolves a node's parent from the bundle's index structure: a page's
// parent is the nearest index above it, which is the parent-before-child anchor.
type hierarchy struct {
	indexByDir map[string]string // dir (rel; "." for root) -> that dir's index rel
}

// srcHierarchy indexes the bundle's index.md pages by directory.
func srcHierarchy(docs []*bundle.Doc) *hierarchy {
	m := map[string]string{}
	for _, d := range docs {
		if d.Kind == bundle.KindIndex {
			m[path.Dir(d.Rel)] = d.Rel
		}
	}
	return &hierarchy{indexByDir: m}
}

// pathHierarchy indexes bare rel paths (scanned nodes) by directory, taking any
// file named index.md as its directory's index.
func pathHierarchy(rels []string) *hierarchy {
	m := map[string]string{}
	for _, rel := range rels {
		if path.Base(rel) == "index.md" {
			m[path.Dir(rel)] = rel
		}
	}
	return &hierarchy{indexByDir: m}
}

// parent returns the symbolic id of rel's parent node — the nearest ancestor
// directory's index — or "" for a node at the top of its area database (no
// covering index above it). An index parents to the next index up, never itself.
func (h *hierarchy) parent(rel string) publish.SymbolicID {
	start := path.Dir(rel)
	if path.Base(rel) == "index.md" {
		start = path.Dir(start) // an index looks strictly above its own directory
	}
	for dir := start; ; dir = path.Dir(dir) {
		if idx, ok := h.indexByDir[dir]; ok && idx != rel {
			return nodeRef(idx)
		}
		if dir == "." || dir == "/" || dir == "" {
			return ""
		}
	}
}

// orphanOps emits a DeleteNode for each vanished subtree root: a scanned node
// with no source, whose parent has not also vanished (an ancestor's single
// DeleteNode archives the whole subtree, so no per-child ops).
func orphanOps(b *bundle.Bundle, cs *publish.CurrentState) []*Op {
	live := map[publish.SymbolicID]bool{}
	for _, d := range b.Docs {
		live[nodeRef(d.Rel)] = true
	}
	var scanned []string
	vanished := map[publish.SymbolicID]bool{}
	for id := range cs.Nodes() {
		scanned = append(scanned, relOfNode(id))
		if !live[id] {
			vanished[id] = true
		}
	}
	h := pathHierarchy(scanned)

	var ops []*Op
	for id := range vanished {
		if p := h.parent(relOfNode(id)); p != "" && vanished[p] {
			continue // covered by the ancestor's subtree deletion
		}
		ops = append(ops, &Op{Kind: DeleteNode, Node: id})
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].Node < ops[j].Node })
	return ops
}

// assembleEdges derives the dependency edges from the op set. Edges are purely
// structural — resolved by symbolic id, never by ordering — so they can be built
// after the concurrent fan-out. There are exactly three causes; a Ref whose
// producer is not among these ops resolves from the scan seed and gets no edge.
func assembleEdges(ops []*Op) []Edge {
	createOf := map[publish.SymbolicID]*Op{}
	anchorProducer := map[publish.AnchorName]*Op{}
	for _, op := range ops {
		switch op.Kind {
		case CreateNode:
			createOf[op.Node] = op
		case SetContent:
			for _, a := range op.Anchors {
				anchorProducer[a] = op
			}
		}
	}

	var edges []Edge
	for _, op := range ops {
		switch op.Kind {
		case CreateNode:
			// #1 parent-before-child: only when the parent is created this run.
			// A parent already in the scan resolves from the seed → no edge.
			if op.Parent == "" {
				continue
			}
			if parentCreate, ok := createOf[op.Parent]; ok {
				edges = append(edges, Edge{From: parentCreate, To: op, Cause: ParentBeforeChild})
			}
		case SetContent:
			for _, ref := range op.Refs {
				if name, ok := anchorRefName(ref); ok {
					// #3 content-refs-anchor. An anchor produced within this very op
					// (a glossary term citing another) resolves in-op — no self-edge.
					if producer, ok := anchorProducer[name]; ok && producer != op {
						edges = append(edges, Edge{From: producer, To: op, Cause: ContentRefsAnchor})
					}
					continue
				}
				// #2 content-refs-node: the edge targets the referent's CreateNode
				// (its existence), which is why mutual links never cycle.
				if create, ok := createOf[ref]; ok && create != op {
					edges = append(edges, Edge{From: create, To: op, Cause: ContentRefsNode})
				}
			}
		}
	}
	sortEdges(edges)
	return edges
}

// sortEdges orders edges deterministically for stable output and tests.
func sortEdges(edges []Edge) {
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		switch {
		case a.Cause != b.Cause:
			return a.Cause < b.Cause
		case a.From.Node != b.From.Node:
			return a.From.Node < b.From.Node
		case a.From.Kind != b.From.Kind:
			return a.From.Kind < b.From.Kind
		case a.To.Node != b.To.Node:
			return a.To.Node < b.To.Node
		default:
			return a.To.Kind < b.To.Kind
		}
	})
}

// relOfNode strips the "node:" scheme from a node symbolic id.
func relOfNode(id publish.SymbolicID) string {
	return strings.TrimPrefix(string(id), "node:")
}

// anchorRefName extracts the anchor name from an "anchor:<name>" symbolic id.
func anchorRefName(id publish.SymbolicID) (publish.AnchorName, bool) {
	if rest, ok := strings.CutPrefix(string(id), "anchor:"); ok {
		return publish.AnchorName(rest), true
	}
	return "", false
}
