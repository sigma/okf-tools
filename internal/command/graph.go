package command

import (
	"encoding/json"
	"fmt"
	"github.com/spf13/pflag"
	"os"
	"strings"

	"github.com/sigma/okf-tools/internal/bundle"
)

// Graph emits the concept link graph as JSON (--format json) or Graphviz dot.
func Graph(args []string) (int, error) {
	fs := pflag.NewFlagSet("graph", pflag.ContinueOnError)
	var g globals
	registerGlobals(fs, &g)
	paths, code, ok := parseFlags(fs, args)
	if !ok {
		return code, nil
	}

	b, err := loadBundle(&g, paths)
	if err != nil {
		return 1, err
	}
	nodes, edges := b.Graph()

	if g.format == "json" {
		type node struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			Type   string `json:"type"`
			Orphan bool   `json:"orphan"`
		}
		out := struct {
			Nodes []node        `json:"nodes"`
			Edges []bundle.Edge `json:"edges"`
		}{Edges: edges}
		if out.Edges == nil {
			out.Edges = []bundle.Edge{}
		}
		for _, d := range nodes {
			out.Nodes = append(out.Nodes, node{ID: d.Rel, Title: d.Title(), Type: d.Type(), Orphan: d.Inbound == 0})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return 0, enc.Encode(out)
	}

	// Graphviz dot.
	var sb strings.Builder
	sb.WriteString("digraph okf {\n")
	sb.WriteString("  rankdir=LR;\n")
	sb.WriteString("  node [shape=box];\n")
	for _, d := range nodes {
		attrs := fmt.Sprintf("label=%q", d.Title())
		if d.Inbound == 0 {
			attrs += ", style=dashed"
		}
		sb.WriteString(fmt.Sprintf("  %q [%s];\n", d.Rel, attrs))
	}
	for _, e := range edges {
		sb.WriteString(fmt.Sprintf("  %q -> %q;\n", e.From, e.To))
	}
	sb.WriteString("}\n")
	fmt.Fprint(os.Stdout, sb.String())
	return 0, nil
}
