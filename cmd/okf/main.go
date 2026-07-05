// Command okf is a small, deterministic CLI for authoring and maintaining Open
// Knowledge Format (OKF) bundles. See docs/DESIGN.md and docs/RULES.md.
package main

import (
	"fmt"
	"os"

	"github.com/sigma/okf-tools/internal/command"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	cmd, args := os.Args[1], os.Args[2:]
	var run func([]string) (int, error)
	switch cmd {
	case "lint":
		run = command.Lint
	case "index":
		run = command.Index
	case "fmt":
		run = command.Fmt
	case "new":
		run = command.New
	case "graph":
		run = command.Graph
	case "version", "--version", "-v":
		fmt.Println("okf " + version)
		return
	case "help", "-h", "--help":
		usage(os.Stdout)
		return
	default:
		fmt.Fprintf(os.Stderr, "okf: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}

	code, err := run(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "okf: "+err.Error())
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func usage(w *os.File) {
	fmt.Fprint(w, `okf — Open Knowledge Format bundle tools

Usage:
  okf <command> [flags] [paths...]

Commands:
  lint    Run the rule catalog (OKF001–OKF206) over the bundle.
  index   Verify (--check) or regenerate (--write) index.md files.
  fmt     Normalize frontmatter, timestamps, citations and link style.
  new     Scaffold a conformant concept page.
  graph   Emit the concept link graph (--format json|dot).
  version Print the version.

Global flags:
  --bundle <dir>    Bundle root (default: auto-discover).
  --config <path>   Config file (default: okf.toml at bundle root).
  --format <fmt>    Output format: human|json (graph also: dot).

Run "okf <command> -h" for command-specific flags.
`)
}
