// Command okfpub mirrors an OKF bundle into a pluggable publishing backend
// (Notion first). It is the second binary in the github.com/sigma/okf-tools
// module and the sole importer of the internal/publish subtree, keeping the
// lint binary (okftool) lean. See sigma/ideas#172.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish/pipeline"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "run":
		if err := runCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "okfpub: "+err.Error())
			os.Exit(1)
		}
	case "version", "--version", "-v":
		fmt.Println("okfpub " + version)
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "okfpub: unknown command %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
}

// runCmd is the minimal `okfpub run` command surface: it resolves the config
// contract (areas.json, schema.json, NOTION_TOKEN, NOTION_DB_ID), selects a
// backend, and drives Generation → Optimization → Transport for one publish. The
// flag surface is deliberately minimal — just enough to select a backend, point at
// the config files, and read the credentials; the full flag design is downstream
// (sigma/ideas#172, "Out of Scope").
func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	backendName := fs.String("backend", string(pipeline.BackendNotion), "publishing backend: notion|fake")
	bundleDir := fs.String("bundle", ".", "bundle root (or a dir to search upward from)")
	configPath := fs.String("config", "", "okf.toml path (default: discovered)")
	areasPath := fs.String("areas", "", "areas.json path (default: <root>/areas.json if present)")
	schemaPath := fs.String("schema", "", "schema.json path (default: <root>/schema.json if present)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Discover + load the bundle through the shared front-end (same parser lint
	// uses); Discover resolves the root that defaults the config-file paths.
	root, cfgPath, err := bundle.Discover(*bundleDir, *bundleDir, *configPath)
	if err != nil {
		return err
	}
	b, err := bundle.Load(root, cfgPath)
	if err != nil {
		return err
	}

	// Resolve the config surface. areas.json / schema.json default to the bundle
	// root but are optional; the credentials come from --* args or the environment.
	cfg, err := pipeline.LoadConfig(pipeline.LoadOptions{
		AreasPath:  defaultPath(*areasPath, b.Root, "areas.json"),
		SchemaPath: defaultPath(*schemaPath, b.Root, "schema.json"),
	})
	if err != nil {
		return err
	}

	kind := pipeline.BackendKind(*backendName)
	be, err := pipeline.SelectBackend(kind, cfg)
	if err != nil {
		return err
	}

	// Echo the resolved config surface so a scheduled run's log shows what contract
	// it published against — and so a mis-pointed --areas/--schema is visible.
	if host, ok := cfg.GlossaryFile(); ok {
		fmt.Printf("okfpub: glossary/anchor host: %s (areas.json role marker)\n", host)
	}
	if cfg.Schema != nil {
		fmt.Printf("okfpub: schema: %d column(s)\n", len(cfg.Schema.Columns))
	}

	res, err := pipeline.Run(context.Background(), be, b)
	if err != nil {
		return err
	}

	fmt.Printf("okfpub: published %d node(s), %d anchor(s) in %d transaction(s) via %s backend\n",
		len(res.Nodes), len(res.Anchors), res.TxnCount, kind)
	return nil
}

// defaultPath returns explicit when set, else <root>/name when that file exists,
// else "" (the loader skips an empty path — both files are optional).
func defaultPath(explicit, root, name string) string {
	if explicit != "" {
		return explicit
	}
	p := filepath.Join(root, name)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

func usage(w *os.File) {
	fmt.Fprint(w, `okfpub — publish an Open Knowledge Format bundle to a backend

Usage:
  okfpub <command> [flags]

Commands:
  run      Publish the bundle to a backend (--backend notion|fake).
  version  Print the version.

Run flags:
  --backend  notion|fake            (default notion)
  --bundle   bundle root            (default ".")
  --config   okf.toml path          (default: discovered)
  --areas    areas.json path        (default: <root>/areas.json if present)
  --schema   schema.json path       (default: <root>/schema.json if present)

Environment:
  NOTION_TOKEN  Notion integration token (required by the notion backend)
  NOTION_DB_ID  Notion data-source id    (required by the notion backend)
`)
}
