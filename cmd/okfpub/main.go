// Command okfpub mirrors an OKF bundle into a pluggable publishing backend
// (Notion first). It is the second binary in the github.com/sigma/okf-tools
// module and the sole importer of the internal/publish subtree, keeping the
// lint binary (okftool) lean. See sigma/ideas#172.
package main

import (
	"fmt"
	"os"

	// The publisher-only subtree is wired here — okfpub is its sole importer, the
	// arrangement the lean-lint-binary invariant protects (see internal/publish's
	// boundary test). These blank imports keep the stubs live and make that "only
	// okfpub" relationship real today; as the stages grow real entry points they
	// become ordinary imports.
	_ "github.com/sigma/okf-tools/internal/publish/backend"
	_ "github.com/sigma/okf-tools/internal/publish/backend/notion"
	_ "github.com/sigma/okf-tools/internal/publish/graph"
	_ "github.com/sigma/okf-tools/internal/publish/optimize"
	_ "github.com/sigma/okf-tools/internal/publish/transport"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println("okfpub " + version)
		return
	case "help", "-h", "--help":
		usage(os.Stdout)
		return
	default:
		fmt.Fprintf(os.Stderr, "okfpub: unknown command %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `okfpub — publish an Open Knowledge Format bundle to a backend

Usage:
  okfpub <command> [flags]

Commands:
  version Print the version.
`)
}
