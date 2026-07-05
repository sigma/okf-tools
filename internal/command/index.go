package command

import (
	"flag"
	"fmt"
	"os"

	"github.com/sigma/okf-tools/internal/rules"
)

// Index verifies (--check, default) or regenerates (--write) index.md files.
func Index(args []string) (int, error) {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	var g globals
	registerGlobals(fs, &g)
	write := fs.Bool("write", false, "regenerate index.md files in place")
	fs.Bool("check", false, "verify indexes are in sync (default)")
	paths, code, ok := parseFlags(fs, args)
	if !ok {
		return code, nil
	}

	b, err := loadBundle(&g, paths)
	if err != nil {
		return 1, err
	}

	if *write {
		n, err := applyFixes(b, fixOptions{Index: true})
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(os.Stdout, "regenerated %d index file(s)\n", n)
		return 0, nil
	}

	// --check: run the index-sync rule (OKF106) only.
	findings := rules.Run(&rules.Context{Bundle: b, Config: b.Config}, map[string]bool{"OKF106": true}, nil)
	if err := renderFindings(os.Stdout, g.format, b, findings); err != nil {
		return 1, err
	}
	if len(findings) > 0 {
		return 1, nil
	}
	return 0, nil
}
