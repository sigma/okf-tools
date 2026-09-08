// Command okftool is a small, deterministic CLI for authoring and maintaining
// Open Knowledge Format (OKF) bundles. See docs/DESIGN.md and docs/RULES.md.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/sigma/okf-tools/internal/climain"
	"github.com/sigma/okf-tools/internal/command"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	app := climain.App{
		Prog:    "okftool",
		Version: version,
		Usage:   usage,
		Commands: map[string]climain.Runner{
			"lint":  command.Lint,
			"index": command.Index,
			"fmt":   command.Fmt,
			"new":   command.New,
			"graph": command.Graph,
			"gaps":  command.Gaps,
			"skill": command.Skill,
		},
	}
	os.Exit(app.Run(os.Stdout, os.Stderr, os.Args[1:]))
}

func usage(w io.Writer) {
	fmt.Fprint(w, `okftool — Open Knowledge Format bundle tools

Usage:
  okftool <command> [flags] [paths...]

Commands:
  lint    Run the rule catalog (OKF001–OKF206) over the bundle.
  index   Verify (--check) or regenerate (--write) index.md files.
  fmt     Normalize frontmatter, timestamps, citations and link style.
  new     Scaffold a conformant concept page.
  graph   Emit the concept link graph (--format json|dot).
  gaps    Concepts semantically near <concept> but not linked to it (needs qmd).
  skill   Print the bundled agent skill (SKILL.md) to stdout.
  version Print the version.

Global flags:
  --bundle <dir>    Bundle root (default: auto-discover).
  --config <path>   Config file (default: okf.toml at bundle root).
  --format <fmt>    Output format: human|json (graph also: dot).

Run "okftool <command> -h" for command-specific flags.
`)
}
