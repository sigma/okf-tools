package command

import (
	"fmt"
	"io"
	"strings"

	"github.com/sigma/okf-tools/internal/gaps"
	"github.com/spf13/pflag"
)

// Gaps reports concepts semantically near the seed but not linked to it — the
// candidate cross-links / bridges to refine a topic. Detection is deterministic;
// composing the bridge is the agent's job. The algorithm lives in internal/gaps;
// this command only parses flags, merges config, and renders the result.
func Gaps(w io.Writer, args []string) (int, error) {
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
	p := gaps.Params{Depth: gc.Depth, Top: gc.Top, MinSim: gc.MinSim, Exclude: typeSetFrom(gc.ExcludeTypes)}
	if fs.Changed("depth") {
		p.Depth = *depthFlag
	}
	if fs.Changed("top") {
		p.Top = *topFlag
	}
	if fs.Changed("min-sim") {
		p.MinSim = *minSimFlag
	}
	if fs.Changed("exclude-types") {
		p.Exclude = parseTypeSet(*excludeFlag)
	}
	if p.Depth != "direct" && p.Depth != "neighborhood" {
		return 2, fmt.Errorf("--depth must be 'direct' or 'neighborhood'")
	}

	seed := b.ResolveWikilink(rest[0])
	if seed == nil {
		return 1, fmt.Errorf("concept not found in the bundle: %s", rest[0])
	}

	res, err := gaps.Find(b, seed, p)
	if err != nil {
		return 1, err
	}
	if g.format == "json" {
		return 0, emitJSON(w, res)
	}
	renderGapsHuman(w, res)
	return 0, nil
}

func renderGapsHuman(w io.Writer, res *gaps.Result) {
	fmt.Fprintf(w, "seed: %s\n", res.Seed)
	if len(res.SeedLinks) > 0 {
		fmt.Fprintf(w, "existing links: %s\n", strings.Join(res.SeedLinks, ", "))
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
