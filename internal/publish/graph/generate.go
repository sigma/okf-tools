package graph

import (
	"context"
	"maps"
	"path"
	"sort"
	"sync"

	"github.com/sigma/okf-tools/internal/areas"
	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish"
)

// Option configures Generate.
type Option func(*options)

type options struct {
	hash   func(*bundle.Doc) publish.Hash
	banner *Banner
	// customHasher records that WithHasher installed a backend hasher that already
	// accounts for the banner in its own projection (the notion recompute hasher
	// hashes the realized block-0 quote), so Generate must not double-fold the
	// banner into the hash the way it does for the default ContentHash.
	customHasher bool
	// selection, when set, narrows this run to the pages it reports true for.
	selection func(rel string) bool
	// areaRoots adds each area's root README.md back to the published node set.
	areaRoots bool
}

// Recomputer is an OPTIONAL backend role: a backend whose live scan can
// reconstruct a content hash implements it, so the pipeline can install a
// source-side hasher (via WithHasher) that hashes the same realized block stream
// the scanner rebuilds — keeping --recompute change detection comparing like
// against like instead of re-clobbering every node (#110). It is deliberately NOT
// part of the backend.Backend umbrella: a backend with no live-reconstruction
// capability (the fake, the filesystem export) simply omits it and the pipeline
// skips the step via a type assertion, exactly as it does for backend.Provisioner.
// The role lives here, beside WithHasher, because its currency — a *Banner and a
// *bundle.Doc — is graph's, not the backend package's neutral publish currency.
type Recomputer interface {
	RecomputeContentHasher(*Banner) func(*bundle.Doc) publish.Hash
}

// WithHasher overrides the expected-content hasher used for change detection, so
// a backend whose scanner reconstructs a matching hash can keep both sides in
// lockstep. Such a hasher owns banner handling itself (it hashes the realized
// block list, block-0 included), so Generate skips the default banner fold for it.
// The default is ContentHash.
func WithHasher(fn func(*bundle.Doc) publish.Hash) Option {
	return func(o *options) {
		if fn != nil {
			o.hash = fn
			o.customHasher = true
		}
	}
}

// WithBanner injects a generated-page disclaimer banner as block-0 of every
// published page and folds its rendered text and per-page source deep-link into
// the content hash, so a banner copy or URL change re-publishes affected pages and
// an unchanged one still hash-skips (sigma/ideas ADR-0015). A nil banner (the
// default) injects nothing, leaving generation unchanged. The banner's source-repo
// coordinates are resolved by the bins (internal/publish/source) and passed in as
// data, so the planner stays environment-free.
// AreaRootPublisher is an OPTIONAL backend role: a backend that publishes an
// area's own root README.md implements it, and the pipeline threads WithAreaRoots
// into Generation on its behalf.
//
// It exists because the exclusion it reverses is Notion-specific — an area maps to
// the unified database, so its landing README is not a row in it — while a
// document-shaped backend has no rows and wants that page as its first tab
// (sigma/okf-tools#163). Like Recomputer, it is deliberately outside the
// backend.Backend umbrella: a backend that does not implement it simply keeps the
// existing scope, and the CLI never learns which backend it built.
type AreaRootPublisher interface {
	// PublishesAreaRoots reports whether an area's root README.md belongs in this
	// backend's published node set.
	PublishesAreaRoots() bool
}

// WithAreaRoots adds each area's own root README.md to the published node set,
// hoisted ahead of the other pages of its area so a document opens with its
// overview. Unset, the node set is exactly what bundle.PublishDocs returns.
func WithAreaRoots() Option {
	return func(o *options) { o.areaRoots = true }
}

// WithSelection narrows the publish to the pages a selection contains — the
// generation-side half of `--select` (sigma/okf-tools#161).
//
// It sits HERE rather than in bundle.PublishDocs because a selection is a
// property of one RUN, not of the bundle: the same bundle is published as several
// documents in a fan-out, each with a different scope, while PublishDocs encodes
// the bundle's own permanent export scope. Cross-links still resolve against the
// whole tree (bundle.Load), so a link out of the selection is a resolved link to
// a page this run simply does not emit ops for.
//
// Unset publishes everything PublishDocs returns, so the Notion path is untouched.
func WithSelection(contains func(rel string) bool) Option {
	return func(o *options) { o.selection = contains }
}

func WithBanner(bn *Banner) Option {
	return func(o *options) { o.banner = bn }
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
	// Fold the banner into the default content hasher so a banner copy or per-page
	// source-URL change re-publishes the page (ADR-0015): the default ContentHash
	// covers only raw source, not the injected block-0. A custom hasher (WithHasher)
	// already hashes the realized block list including the banner, so it is left
	// alone — folding again would double-count.
	if o.banner != nil && !o.customHasher {
		base, bn := o.hash, o.banner
		o.hash = func(d *bundle.Doc) publish.Hash { return bn.hash(base(d), d.Rel) }
	}
	if cs == nil {
		cs = publish.NewCurrentState(nil, nil, nil)
	}

	// The published node set is scoped to areas.json's declared content areas plus
	// the glossary host (b.PublishDocs). Cross-links are already resolved against
	// the whole tree at load time (bundle.Load), so narrowing here only gates op
	// emission — a link from an in-scope page still resolves. An okf.toml-only
	// bundle has no areas registry and PublishDocs returns every doc, so behaviour
	// there is unchanged.
	docs := b.PublishDocs()
	if o.areaRoots {
		docs = withAreaRoots(docs, b.AreaRootDocs())
	}
	scope := newSelectionScope(o.selection, o.banner)
	if scope != nil {
		kept := docs[:0:0]
		for _, d := range docs {
			if scope.includes(d.Rel) {
				kept = append(kept, d)
			}
		}
		docs = kept
	}

	// Concurrent per-page diff → ops. Results are slotted by index so op order is
	// deterministic (docs preserves b.Docs' rel sort) regardless of goroutine
	// scheduling.
	src := srcHierarchy(docs, b.Areas)
	results := make([]docOps, len(docs))
	var wg sync.WaitGroup
	for i, d := range docs {
		wg.Add(1)
		go func(i int, d *bundle.Doc) {
			defer wg.Done()
			if ctx.Err() != nil {
				return
			}
			results[i] = diffDoc(d, cs, &o, src, scope)
		}(i, d)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// An anchor a page CITES must be resolvable: either the scan already knows it, or
	// something in this run mints it. Where neither holds, re-assert its host even
	// though the host hash-skipped — see repairUnhostedAnchors.
	repairUnhostedAnchors(results, cs)

	g := &Graph{}
	for _, r := range results {
		g.Ops = append(g.Ops, r.ops...)
	}
	// Orphans: scanned nodes with no in-scope source → DeleteNode on the subtree
	// roots. Liveness is the publish set, so a page that fell out of scope (or was
	// leaked by the pre-scoping publisher) reconciles to a deletion.
	g.Ops = append(g.Ops, orphanOps(docs, cs, b.Areas)...)

	g.Edges = assembleEdges(g.Ops)
	return g, nil
}

// docOps is one page's diff result: the ops it emits, plus its content op and
// whether that op was emitted. The content op is reported even when suppressed
// because it is the only thing that mints the page's anchor map, so a later pass may
// need to bring it back — see repairUnhostedAnchors.
type docOps struct {
	ops []*Op
	// content is the page's SetContent op, reported whether or not it was emitted:
	// it is the only thing that mints the page's anchor map, so a later pass may need
	// to bring it back. contentEmitted says which it is, so a caller cannot mistake a
	// suppressed op for a scheduled one.
	content        *Op
	contentEmitted bool
}

// repairUnhostedAnchors re-asserts the host of any anchor this run CITES but cannot
// resolve — neither seeded by the scan nor minted by an op already emitted.
//
// Asserting a page's content is the only thing that mints and records its anchor map,
// so a host whose map was never recorded (a glossary hosted as a subpage, before
// sigma/okf-tools#142 gave subtree entries an anchors field) hash-skips forever while
// every page citing it has a reference the run cannot resolve. The run then aborts
// with "transactions cannot proceed", identically on every retry. This is the escape:
// the citation is the evidence that the map is needed, and the host is re-asserted to
// produce it.
//
// The repair is decided HERE rather than in the scan, which is a deliberate
// departure from what #142 first proposed. A scan cannot see it: a host that
// recorded no anchors is indistinguishable from one that hosts none, and only the
// source says which. Deciding it in Generation also covers the steady-state scan,
// which has no live blocks to reason about at all — where #138's dangling rule,
// which needs them, necessarily lives in the recompute scan.
//
// It is demand-driven on purpose. Re-asserting every host whose anchors happen to be
// unseeded would rewrite pages nothing cites — churn, and exactly the false re-write
// the drift detection is careful to avoid.
//
// The repeat pass exists because a repair can create demand: the content op it brings
// back may itself cite an anchor hosted by a page that has not been re-asserted. Each
// pass can only flip a page from suppressed to emitted, and never back, so it runs at
// most once per page and then stops.
//
// Iteration is over slices, never maps, so the ops a run emits do not depend on map
// ordering — the same determinism the concurrent fan-out above preserves by slotting
// results by index.
func repairUnhostedAnchors(results []docOps, cs *publish.CurrentState) {
	// Which page hosts an anchor. When two pages declare the same name — two glossary
	// files defining one term — the later one wins, matching how assembleEdges picks
	// that anchor's producer. The two must agree: repairing one page while wiring the
	// edge to another would order the run against a producer it never scheduled.
	hostOf := map[publish.AnchorName]int{}
	for i, r := range results {
		if r.content == nil {
			continue // a page whose diff never ran (a cancelled fan-out)
		}
		for _, name := range r.content.Anchors {
			hostOf[name] = i
		}
	}

	for {
		repaired := false
		for _, r := range results {
			for _, op := range r.ops {
				for _, ref := range op.Refs {
					name, ok := ref.AnchorName()
					if !ok {
						continue
					}
					if _, seeded := cs.AnchorID(name); seeded {
						continue
					}
					host, known := hostOf[name]
					// An already-emitted host is producing the anchor this run; a name
					// nothing hosts is a dangling citation the transport reports by name.
					if !known || results[host].contentEmitted {
						continue
					}
					// Only the content op comes back. Properties are gated by their own
					// hash and are not what mints an anchor map, so re-asserting them
					// here would be a rewrite nothing asked for.
					results[host].ops = append(results[host].ops, results[host].content)
					results[host].contentEmitted = true
					repaired = true
				}
			}
		}
		if !repaired {
			return
		}
	}
}

// diffDoc emits the ops for one source page from the uniform diff→ops mapping:
//
//	new              → CreateNode + SetProperties + SetContent
//	changed/drifted  → SetProperties + SetContent
//	unchanged        → nothing (hash-skip)
//
// (Vanished nodes have no source and are handled by orphanOps.)
func diffDoc(d *bundle.Doc, cs *publish.CurrentState, o *options, src *hierarchy, scope *selectionScope) docOps {
	node := publish.NodeRef(d.Rel)
	parent := src.parent(d.Rel)
	owner := src.owner(d.Rel)
	hash := o.hash(d)
	propHash := PropertyHash(d)
	title := d.Title()
	doc, refs, anchors := buildDocument(d, o.banner, scope)
	// Stamp the parent, both expected hashes, and title on every op (not just the
	// create) so publish-time write-back can route and record a touched node — new
	// or re-asserted — and stamp both hash columns from whichever arm survives.
	setProps := &Op{Kind: SetProperties, Node: node, Props: propsOf(d), NodeStamp: publish.NodeStamp{Parent: parent, Owner: owner, Hash: hash, PropHash: propHash, Title: title}}
	setContent := &Op{Kind: SetContent, Node: node, Doc: doc, Refs: refs, Anchors: anchors, NodeStamp: publish.NodeStamp{Parent: parent, Owner: owner, Hash: hash, PropHash: propHash, Title: title}}

	if _, exists := cs.NodeID(node); !exists {
		create := &Op{Kind: CreateNode, Node: node, NodeStamp: publish.NodeStamp{Parent: parent, Owner: owner, Hash: hash, PropHash: propHash, Title: title}}
		return docOps{ops: []*Op{create, setProps, setContent}, content: setContent, contentEmitted: true}
	}
	// Existing: gate the two arms independently, each by its own scanned hash. An arm
	// hash-skips iff its scanned hash is present and equals the expected one; a
	// missing scanned hash cannot confirm "unchanged", so it re-asserts. So a body-
	// only edit emits just SetContent, a title/type-only edit just SetProperties,
	// both edits both arms, and an unchanged node nothing.
	contentGot, cok := cs.ContentHash(node)
	propGot, pok := cs.PropertyHash(node)
	var ops []*Op
	if !(pok && propGot == propHash) {
		ops = append(ops, setProps)
	}
	emitted := !(cok && contentGot == hash)
	if emitted {
		ops = append(ops, setContent)
	}
	return docOps{ops: ops, content: setContent, contentEmitted: emitted}
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

// indexRank ranks a bundle-relative path's role as its directory's nesting
// index, higher wins: index.md is the OKF-native index (2); a cluster's
// README.md is a fallback index (1) so a cluster whose entry point is README.md
// (this repo has no index.md files — OKF reserves index.md for generated nav
// indexes, OKF003) still parents its siblings; anything else is not an index (0).
//
// A README.md whose directory is *itself* a declared areas.json area root is not
// a cluster index (0): that README is the area's section-landing page, which maps
// to the database itself rather than a page within it — it is not published as a
// row at all (bundle.InPublishScope) and never parents siblings. ar may be nil
// (an okf.toml-only bundle), in which case no directory is an area root, so a
// README.md is always a cluster index.
func indexRank(rel string, ar *areas.Registry) int {
	switch path.Base(rel) {
	case "index.md":
		return 2
	case "README.md":
		if ar.IsAreaRoot(path.Dir(rel)) {
			return 0
		}
		return 1
	default:
		return 0
	}
}

// buildHierarchy indexes each directory's nesting index from a set of rels,
// resolving index.md > cluster README.md when a directory has both (indexRank).
func buildHierarchy(rels []string, ar *areas.Registry) *hierarchy {
	m := map[string]string{}
	rank := map[string]int{}
	for _, rel := range rels {
		r := indexRank(rel, ar)
		if r == 0 {
			continue
		}
		dir := path.Dir(rel)
		if r > rank[dir] {
			m[dir], rank[dir] = rel, r
		}
	}
	return &hierarchy{indexByDir: m}
}

// srcHierarchy indexes the bundle's per-directory index pages (index.md, or a
// cluster's README.md) by directory.
func srcHierarchy(docs []*bundle.Doc, ar *areas.Registry) *hierarchy {
	rels := make([]string, len(docs))
	for i, d := range docs {
		rels[i] = d.Rel
	}
	return buildHierarchy(rels, ar)
}

// pathHierarchy indexes bare rel paths (scanned nodes) by directory, applying the
// same index recognition as srcHierarchy so a scanned tree and a source tree
// agree on each directory's index.
func pathHierarchy(rels []string, ar *areas.Registry) *hierarchy {
	return buildHierarchy(rels, ar)
}

// parent returns the symbolic id of rel's parent node — the nearest ancestor
// directory's index — or "" for a node at the top of its area database (no
// covering index above it). A directory's own index parents to the next index
// up, never itself.
func (h *hierarchy) parent(rel string) publish.SymbolicID {
	start := path.Dir(rel)
	if h.indexByDir[start] == rel {
		start = path.Dir(start) // a dir index looks strictly above its own directory
	}
	for dir := start; ; dir = path.Dir(dir) {
		if idx, ok := h.indexByDir[dir]; ok && idx != rel {
			return publish.NodeRef(idx)
		}
		if dir == "." || dir == "/" || dir == "" {
			return ""
		}
	}
}

// owner returns the symbolic id of the nearest ancestor of rel that is a ROW — the
// node whose subtree map records rel — or "" when rel is itself a row.
//
// It walks the same parent chain as parent, but keeps climbing while each ancestor
// is itself page-parented: a mirror may nest arbitrarily deep, and only a node with
// no parent has a data-source row to hold a record (sigma/okf-tools#141). For the
// one-level case — a leaf under a cluster index that is itself top-level — owner and
// parent are the same node, which is why the common layout is unaffected.
func (h *hierarchy) owner(rel string) publish.SymbolicID {
	for cur := rel; ; {
		p := h.parent(cur)
		if p == "" {
			if cur == rel {
				return "" // rel is itself a row
			}
			return publish.NodeRef(cur)
		}
		cur = p.Rel()
	}
}

// orphanOps emits a DeleteNode for each vanished subtree root: a scanned node
// with no source, whose parent has not also vanished (an ancestor's single
// DeleteNode archives the whole subtree, so no per-child ops).
func orphanOps(docs []*bundle.Doc, cs *publish.CurrentState, ar *areas.Registry) []*Op {
	live := map[publish.SymbolicID]bool{}
	for _, d := range docs {
		live[publish.NodeRef(d.Rel)] = true
	}
	var scanned []string
	vanished := map[publish.SymbolicID]bool{}
	// An unclaimed object (a row an aborted run created and never recorded, #135)
	// has no path, so it takes no part in the subtree reasoning below: it can be
	// neither an ancestor nor a descendant of anything. It is reclaimed on its own
	// terms — sourceless by construction, since no source doc can mint its id.
	var unclaimed []publish.SymbolicID
	for id := range cs.Nodes() {
		if _, isUnclaimed := id.Unclaimed(); isUnclaimed {
			unclaimed = append(unclaimed, id)
			continue
		}
		scanned = append(scanned, id.Rel())
		if !live[id] {
			vanished[id] = true
		}
	}
	h := pathHierarchy(scanned, ar)

	var ops []*Op
	for id := range vanished {
		if p := h.parent(id.Rel()); p != "" && vanished[p] {
			continue // covered by the ancestor's subtree deletion
		}
		ops = append(ops, &Op{Kind: DeleteNode, Node: id})
	}
	for _, id := range unclaimed {
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
				if name, ok := ref.AnchorName(); ok {
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

// UnclaimedDeletes reports how many of the graph's ops reclaim an unclaimed backend
// object rather than archive a vanished node (#135). It lives here because it is a
// question about this package's op vocabulary; a caller that reports the number
// should not have to know how a reclaim op is spelled.
func (g *Graph) UnclaimedDeletes() int {
	n := 0
	for _, op := range g.Ops {
		if op.Kind != DeleteNode {
			continue
		}
		if _, ok := op.Node.Unclaimed(); ok {
			n++
		}
	}
	return n
}
