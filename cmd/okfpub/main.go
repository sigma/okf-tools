// Command okfpub mirrors an OKF bundle into a pluggable publishing backend
// (Notion first). It is the second binary in the github.com/sigma/okf-tools
// module and the sole importer of the internal/publish subtree, keeping the
// lint binary (okftool) lean. See sigma/ideas#172.
//
// This file is deliberately the whole of package main: it picks the subcommand,
// supplies the process's stdout, and turns the returned code into an exit status.
// Everything else — flags, the config contract, backend selection, the fan-out,
// the reporting — lives in internal/publishcmd, where a test can drive it.
package main

import (
	"os"

	"github.com/sigma/okf-tools/internal/climain"
	"github.com/sigma/okf-tools/internal/publishcmd"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	app := climain.App{
		Prog:     "okfpub",
		Version:  version,
		Usage:    publishcmd.Usage,
		Commands: map[string]climain.Runner{"run": publishcmd.Run},
	}
	os.Exit(app.Run(os.Stdout, os.Stderr, os.Args[1:]))
}
