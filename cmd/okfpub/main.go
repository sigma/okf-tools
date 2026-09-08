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
	"fmt"
	"io"
	"os"

	"github.com/sigma/okf-tools/internal/publishcmd"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		publishcmd.Usage(os.Stderr)
		os.Exit(2)
	}

	cmd, args := os.Args[1], os.Args[2:]
	var run func(io.Writer, []string) (int, error)
	switch cmd {
	case "run":
		run = publishcmd.Run
	case "version", "--version", "-v":
		fmt.Println("okfpub " + version)
		return
	case "help", "-h", "--help":
		publishcmd.Usage(os.Stdout)
		return
	default:
		fmt.Fprintf(os.Stderr, "okfpub: unknown command %q\n\n", cmd)
		publishcmd.Usage(os.Stderr)
		os.Exit(2)
	}

	code, err := run(os.Stdout, args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "okfpub: "+err.Error())
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
