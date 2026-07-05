package command

import (
	"encoding/json"
	"fmt"
	"github.com/spf13/pflag"
	"os"
)

// Fmt normalizes frontmatter key order, timestamps, citation numbering and link
// style. --check reports which files would change (non-zero exit if any);
// --write applies them.
func Fmt(args []string) (int, error) {
	fs := pflag.NewFlagSet("fmt", pflag.ContinueOnError)
	var g globals
	registerGlobals(fs, &g)
	write := fs.Bool("write", false, "rewrite files in place")
	fs.Bool("check", false, "report files that are not formatted (default)")
	paths, code, ok := parseFlags(fs, args)
	if !ok {
		return code, nil
	}
	if err := validateFormat(g.format, "human", "json"); err != nil {
		return 2, err
	}

	b, err := loadBundle(&g, paths)
	if err != nil {
		return 1, err
	}

	opts := fixOptions{Frontmatter: true, Timestamp: true, Citations: true}
	if b.Config.Links.Style == "relative" || b.Config.Links.Style == "absolute" {
		opts.LinkStyle = true
	}

	if *write {
		n, err := applyFixes(b, opts)
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(os.Stdout, "formatted %d file(s)\n", n)
		return 0, nil
	}

	var changed []string
	for _, d := range b.Concepts {
		if fixDoc(b, d, opts) != d.Content {
			changed = append(changed, d.Rel)
		}
	}
	if err := renderChanged(g.format, changed); err != nil {
		return 1, err
	}
	if len(changed) > 0 {
		return 1, nil
	}
	return 0, nil
}

func renderChanged(format string, changed []string) error {
	if format == "json" {
		if changed == nil {
			changed = []string{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"would_reformat": changed})
	}
	for _, p := range changed {
		fmt.Fprintf(os.Stdout, "would reformat %s\n", p)
	}
	fmt.Fprintf(os.Stdout, "%d file(s) need formatting\n", len(changed))
	return nil
}
