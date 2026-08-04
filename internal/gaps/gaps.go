// Package gaps finds concepts semantically near a seed page but not yet linked
// to it — the candidate cross-links / bridges a bundle author might add to refine
// a topic. Detection is deterministic; composing the bridge is the agent's job.
package gaps

import (
	"sort"
	"strings"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/qmd"
)

// Params configures a gaps search. Runner is the qmd invoker: leave it nil in
// production (qmd resolves the real binary from config) and inject a fake in
// tests — the dependency is accepted here, not created inside the algorithm.
type Params struct {
	Depth   string          // "direct" | "neighborhood"
	Top     int             // neighbors to consider
	MinSim  float64         // similarity floor
	Exclude map[string]bool // node types to skip, lowercased
	Runner  qmd.Runner
}

// Neighbor is a concept near the seed and whether it is already cross-linked.
type Neighbor struct {
	Page   string  `json:"page"`
	Sim    float64 `json:"sim"`
	Linked bool    `json:"linked"`
}

// Direct is a neighbor that is near the seed but not linked to it — a candidate
// direct cross-link.
type Direct struct {
	Page string  `json:"page"`
	Sim  float64 `json:"sim"`
}

// Hole is a pair among the seed's neighbors that are mutually near yet unlinked —
// an open triangle / local structural hole.
type Hole struct {
	A   string  `json:"a"`
	B   string  `json:"b"`
	Sim float64 `json:"sim"`
}

// Result is the complete, self-describing output of a gaps search: everything a
// renderer needs without re-querying the bundle. SeedLinks lists the seed's
// existing links for human output only and stays off the JSON wire.
type Result struct {
	Seed      string     `json:"seed"`
	Neighbors []Neighbor `json:"neighbors"`
	Direct    []Direct   `json:"direct"`
	Holes     []Hole     `json:"holes,omitempty"`
	SeedLinks []string   `json:"-"`
}

// Find reports concepts semantically near the seed but not linked to it. With
// Depth == "neighborhood" it also finds unlinked pairs among those neighbors
// (open triangles), at the cost of one extra qmd query per neighbor.
func Find(b *bundle.Bundle, seed *bundle.Doc, p Params) (*Result, error) {
	concepts := qmdConcepts(b)
	docByRel := conceptsByRel(b)
	isLinked := linkChecker(b)

	ns, err := qmd.Neighbors(b.Root, seedQuery(seed), concepts, p.MinSim, &b.Config.QMD, p.Runner)
	if err != nil {
		return nil, err
	}

	kept := filterNeighbors(ns, seed.Rel, docByRel, p.Exclude, p.Top)
	res := &Result{Seed: seed.Rel, SeedLinks: seedLinks(b, seed.Rel)}
	for _, n := range kept {
		l := isLinked(seed.Rel, n.Rel)
		res.Neighbors = append(res.Neighbors, Neighbor{Page: n.Rel, Sim: n.Score, Linked: l})
		if !l {
			res.Direct = append(res.Direct, Direct{Page: n.Rel, Sim: n.Score})
		}
	}

	if p.Depth == "neighborhood" {
		holes, err := neighborhoodHoles(b, kept, docByRel, isLinked, p.MinSim, p.Runner)
		if err != nil {
			return nil, err
		}
		res.Holes = holes
	}
	return res, nil
}

// filterNeighbors drops the seed itself and excluded types, then keeps top k.
func filterNeighbors(ns []qmd.Neighbor, seedRel string, docByRel map[string]*bundle.Doc, exclude map[string]bool, top int) []qmd.Neighbor {
	var kept []qmd.Neighbor
	for _, n := range ns {
		if n.Rel == seedRel {
			continue
		}
		if d := docByRel[n.Rel]; d != nil && exclude[strings.ToLower(d.Type())] {
			continue
		}
		kept = append(kept, n)
		if len(kept) >= top {
			break
		}
	}
	return kept
}

// neighborhoodHoles finds pairs among the seed's neighbors that are mutually near
// (one qmd query per neighbor) yet unlinked — open triangles / local structural
// holes. Costs +k queries.
func neighborhoodHoles(b *bundle.Bundle, neighbors []qmd.Neighbor, docByRel map[string]*bundle.Doc, isLinked func(a, x string) bool, minSim float64, runner qmd.Runner) ([]Hole, error) {
	concepts := qmdConcepts(b)
	inSet := map[string]bool{}
	for _, n := range neighbors {
		inSet[n.Rel] = true
	}
	seen := map[string]bool{}
	var holes []Hole
	for _, a := range neighbors {
		ad := docByRel[a.Rel]
		if ad == nil {
			continue
		}
		aNs, err := qmd.Neighbors(b.Root, seedQuery(ad), concepts, minSim, &b.Config.QMD, runner)
		if err != nil {
			continue
		}
		aScore := map[string]float64{}
		for _, n := range aNs {
			aScore[n.Rel] = n.Score
		}
		for _, x := range neighbors {
			if x.Rel == a.Rel {
				continue
			}
			s, near := aScore[x.Rel]
			if !near || isLinked(a.Rel, x.Rel) {
				continue
			}
			lo, hi := orderedPair(a.Rel, x.Rel)
			key := lo + "\x00" + hi
			if seen[key] {
				continue
			}
			seen[key] = true
			holes = append(holes, Hole{A: lo, B: hi, Sim: s})
		}
	}
	sort.Slice(holes, func(i, j int) bool {
		if holes[i].A != holes[j].A {
			return holes[i].A < holes[j].A
		}
		return holes[i].B < holes[j].B
	})
	return holes, nil
}

// seedQuery builds the qmd query text for a concept: title + description, or
// title + body when the page has no description (thin frontmatter).
func seedQuery(d *bundle.Doc) string {
	q := d.Title()
	if desc := d.Description(); desc != "" {
		return strings.TrimSpace(q + ". " + desc)
	}
	return strings.TrimSpace(q + "\n\n" + d.Body)
}

func conceptsByRel(b *bundle.Bundle) map[string]*bundle.Doc {
	m := make(map[string]*bundle.Doc, len(b.Concepts))
	for _, d := range b.Concepts {
		m[d.Rel] = d
	}
	return m
}

// linkChecker returns an undirected "are these two concepts cross-linked" test.
func linkChecker(b *bundle.Bundle) func(a, x string) bool {
	_, edges := b.Graph()
	linked := map[string]bool{}
	for _, e := range edges {
		lo, hi := orderedPair(e.From, e.To)
		linked[lo+"\x00"+hi] = true
	}
	return func(a, x string) bool {
		lo, hi := orderedPair(a, x)
		return linked[lo+"\x00"+hi]
	}
}

// seedLinks lists the concepts the seed is cross-linked to, sorted.
func seedLinks(b *bundle.Bundle, seedRel string) []string {
	_, edges := b.Graph()
	set := map[string]bool{}
	for _, e := range edges {
		if e.From == seedRel {
			set[e.To] = true
		}
		if e.To == seedRel {
			set[e.From] = true
		}
	}
	var out []string
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// qmdConcepts adapts the bundle's concepts to the qmd package's input. It mirrors
// the command layer's adapter of the same name: both sit above the bundle↔qmd
// seam (qmd cannot import bundle), so the tiny adapter is duplicated rather than
// shared through a third package.
func qmdConcepts(b *bundle.Bundle) []qmd.Concept {
	out := make([]qmd.Concept, 0, len(b.Concepts))
	for _, d := range b.Concepts {
		out = append(out, qmd.Concept{Rel: d.Rel, Abs: d.Path, Text: d.Body})
	}
	return out
}

func orderedPair(a, b string) (string, string) {
	if a > b {
		return b, a
	}
	return a, b
}
