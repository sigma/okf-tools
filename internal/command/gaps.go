package command

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/qmd"
	"github.com/spf13/pflag"
)

// gapsRunner is nil in production (qmd resolves the real binary); tests set it.
var gapsRunner qmd.Runner

type gapNeighbor struct {
	Page   string  `json:"page"`
	Sim    float64 `json:"sim"`
	Linked bool    `json:"linked"`
}

type gapDirect struct {
	Page string  `json:"page"`
	Sim  float64 `json:"sim"`
}

type gapHole struct {
	A   string  `json:"a"`
	B   string  `json:"b"`
	Sim float64 `json:"sim"`
}

type gapsResult struct {
	Seed      string        `json:"seed"`
	Neighbors []gapNeighbor `json:"neighbors"`
	Direct    []gapDirect   `json:"direct"`
	Holes     []gapHole     `json:"holes,omitempty"`
}

// Gaps reports concepts semantically near the seed but not linked to it — the
// candidate cross-links / bridges to refine a topic. Detection is deterministic;
// composing the bridge is the agent's job.
func Gaps(args []string) (int, error) {
	fs := pflag.NewFlagSet("gaps", pflag.ContinueOnError)
	var g globals
	registerGlobals(fs, &g)
	// Flags override the config defaults ([gaps]); unset flags fall back to config.
	depthFlag := fs.String("depth", "", "direct|neighborhood (default: config gaps.depth)")
	topFlag := fs.Int("top", 0, "neighbors to consider (default: config gaps.top)")
	minSimFlag := fs.Float64("min-sim", 0, "similarity floor (default: config gaps.min_sim)")
	excludeFlag := fs.String("exclude-types", "", "skip node types, comma-separated (e.g. Person)")
	rest, code, ok := parseFlags(fs, args)
	if !ok {
		return code, nil
	}
	if len(rest) != 1 {
		return 2, fmt.Errorf("usage: okftool gaps <concept> [flags]")
	}
	if err := validateFormat(g.format, "human", "json"); err != nil {
		return 2, err
	}

	b, err := loadBundle(&g, nil)
	if err != nil {
		return 1, err
	}
	if !b.Config.QMD.Enabled {
		return 1, fmt.Errorf("gaps requires qmd; set qmd.enabled = true in okf.toml")
	}

	gc := b.Config.Gaps
	depth, top, minSim := gc.Depth, gc.Top, gc.MinSim
	exclude := typeSetFrom(gc.ExcludeTypes)
	if fs.Changed("depth") {
		depth = *depthFlag
	}
	if fs.Changed("top") {
		top = *topFlag
	}
	if fs.Changed("min-sim") {
		minSim = *minSimFlag
	}
	if fs.Changed("exclude-types") {
		exclude = parseTypeSet(*excludeFlag)
	}
	if depth != "direct" && depth != "neighborhood" {
		return 2, fmt.Errorf("--depth must be 'direct' or 'neighborhood'")
	}

	seed := b.ResolveWikilink(rest[0])
	if seed == nil {
		return 1, fmt.Errorf("concept not found in the bundle: %s", rest[0])
	}

	res, err := computeGaps(b, seed, depth, top, minSim, exclude)
	if err != nil {
		return 1, err
	}
	if g.format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return 0, enc.Encode(res)
	}
	renderGapsHuman(os.Stdout, b, seed, res)
	return 0, nil
}

func computeGaps(b *bundle.Bundle, seed *bundle.Doc, depth string, top int, minSim float64, exclude map[string]bool) (*gapsResult, error) {
	concepts := qmdConcepts(b)
	docByRel := conceptsByRel(b)
	isLinked := linkChecker(b)

	ns, err := qmd.Neighbors(b.Root, seedQuery(seed), concepts, minSim, &b.Config.QMD, gapsRunner)
	if err != nil {
		return nil, err
	}

	kept := filterNeighbors(ns, seed.Rel, docByRel, exclude, top)
	res := &gapsResult{Seed: seed.Rel}
	for _, n := range kept {
		l := isLinked(seed.Rel, n.Rel)
		res.Neighbors = append(res.Neighbors, gapNeighbor{Page: n.Rel, Sim: n.Score, Linked: l})
		if !l {
			res.Direct = append(res.Direct, gapDirect{Page: n.Rel, Sim: n.Score})
		}
	}

	if depth == "neighborhood" {
		holes, err := neighborhoodHoles(b, kept, docByRel, isLinked, minSim)
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
func neighborhoodHoles(b *bundle.Bundle, neighbors []qmd.Neighbor, docByRel map[string]*bundle.Doc, isLinked func(a, x string) bool, minSim float64) ([]gapHole, error) {
	concepts := qmdConcepts(b)
	inSet := map[string]bool{}
	for _, n := range neighbors {
		inSet[n.Rel] = true
	}
	seen := map[string]bool{}
	var holes []gapHole
	for _, a := range neighbors {
		ad := docByRel[a.Rel]
		if ad == nil {
			continue
		}
		aNs, err := qmd.Neighbors(b.Root, seedQuery(ad), concepts, minSim, &b.Config.QMD, gapsRunner)
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
			holes = append(holes, gapHole{A: lo, B: hi, Sim: s})
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

func renderGapsHuman(w io.Writer, b *bundle.Bundle, seed *bundle.Doc, res *gapsResult) {
	fmt.Fprintf(w, "seed: %s\n", res.Seed)
	if links := seedLinks(b, seed.Rel); len(links) > 0 {
		fmt.Fprintf(w, "existing links: %s\n", strings.Join(links, ", "))
	} else {
		fmt.Fprintln(w, "existing links: (none)")
	}
	fmt.Fprintln(w, "neighbors:")
	for _, n := range res.Neighbors {
		tag := "GAP"
		if n.Linked {
			tag = "ok "
		}
		fmt.Fprintf(w, "  %s  %-44s %.2f\n", tag, n.Page, n.Sim)
	}
	if len(res.Direct) > 0 {
		fmt.Fprintln(w, "direct gaps (near but unlinked):")
		for _, d := range res.Direct {
			fmt.Fprintf(w, "  %-44s %.2f\n", d.Page, d.Sim)
		}
	}
	if len(res.Holes) > 0 {
		fmt.Fprintln(w, "neighborhood holes (unlinked pairs among the neighbors):")
		for _, h := range res.Holes {
			fmt.Fprintf(w, "  %s -- %s  %.2f\n", h.A, h.B, h.Sim)
		}
	}
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

func parseTypeSet(csv string) map[string]bool {
	return typeSetFrom(strings.Split(csv, ","))
}

func typeSetFrom(types []string) map[string]bool {
	set := map[string]bool{}
	for _, t := range types {
		if tt := strings.ToLower(strings.TrimSpace(t)); tt != "" {
			set[tt] = true
		}
	}
	return set
}

func orderedPair(a, b string) (string, string) {
	if a > b {
		return b, a
	}
	return a, b
}
