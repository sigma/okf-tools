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
	"os/exec"
	"path/filepath"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/backend/notion"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/pipeline"
	"github.com/sigma/okf-tools/internal/publish/source"
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
	backendName := fs.String("backend", string(pipeline.BackendNotion), "publishing backend: notion|fake|fs")
	bundleDir := fs.String("bundle", ".", "bundle root (or a dir to search upward from)")
	configPath := fs.String("config", "", "okf.toml path (default: discovered)")
	areasPath := fs.String("areas", "", "areas.json path (default: <root>/areas.json if present)")
	schemaPath := fs.String("schema", "", "schema.json path (default: <root>/schema.json if present)")
	outDir := fs.String("out", "", "output dir for the fs/export backend (default: "+pipeline.DefaultOutDir+")")
	dryRun := fs.Bool("dry-run", false, "export to the filesystem instead of publishing (implies --backend fs)")
	recompute := fs.Bool("recompute", false, "opt into the full live-block scan (true drift + subpage/anchor self-heal); default is the cheap steady-state scan")
	interval := fs.Duration("interval", notion.DefaultInterval, "minimum spacing between Notion writes (reads burst ahead of it); zero or less disables pacing")
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
	cfg.OutDir = *outDir
	cfg.NotionInterval = interval

	ctx := context.Background()

	// --dry-run is sugar for --backend fs: export to the filesystem instead of
	// touching a live workspace.
	kind := pipeline.BackendKind(*backendName)
	if *dryRun {
		kind = pipeline.BackendFS
	}
	be, err := pipeline.SelectBackend(ctx, kind, cfg, filepath.Base(b.Root))
	if err != nil {
		return err
	}
	// Echo a non-default pacing choice, but only where it applies: a run that is
	// unusually fast or slow should say so in its own log rather than leave the
	// reader to guess at the operator's flags. The fs/fake backends pace nothing, so
	// printing it there would describe a knob that did not turn.
	if kind == pipeline.BackendNotion && *interval != notion.DefaultInterval {
		fmt.Printf("okfpub: notion pacing: %v between writes\n", *interval)
	}

	// Echo the resolved config surface so a scheduled run's log shows what contract
	// it published against — and so a mis-pointed --areas/--schema is visible.
	if host, ok := cfg.GlossaryFile(); ok {
		fmt.Printf("okfpub: glossary/anchor host: %s (areas.json role marker)\n", host)
	}
	if cfg.Schema != nil {
		fmt.Printf("okfpub: schema: %d column(s)\n", len(cfg.Schema.Columns))
	}

	var runOpts []pipeline.Option
	if *recompute {
		runOpts = append(runOpts, pipeline.WithScanMode(backend.ScanRecompute))
	}

	// Resolve the generated-page disclaimer banner (ADR-0015). Resolution reads the
	// environment and, as a fallback, local git — the bin's job, kept out of the
	// pure planner — and is threaded into Generation as data. A misconfigured source
	// URL fails loud rather than publishing a banner with a dangling deep-link.
	if bn := b.Config.Banner; bn.Enabled {
		src, err := source.Resolve(os.Getenv, gitIn(b.Root))
		if err != nil {
			return err
		}
		runOpts = append(runOpts, pipeline.WithBanner(&graph.Banner{
			Text:    bn.Text,
			BaseURL: src.BaseURL,
			Ref:     src.Ref,
			Prefix:  src.Prefix,
		}))
		// Echo the prefix too: a bundle nested in its repo emits links that are only
		// correct with it, so a run that silently resolved none is worth seeing.
		fmt.Printf("okfpub: banner: source %s @ %s (bundle at %s)\n",
			src.BaseURL, src.Ref, prefixLabel(src.Prefix))
	}

	res, err := pipeline.Run(ctx, be, b, runOpts...)
	if err != nil {
		return err
	}

	fmt.Printf("okfpub: published %d node(s), %d anchor(s) in %d transaction(s) via %s backend\n",
		len(res.Nodes), len(res.Anchors), res.TxnCount, kind)
	// The traffic line is what makes a slow run diagnosable without a debugger: a
	// run whose request count dwarfs its transaction count is doing per-request work
	// nobody planned, and one whose retries dominate is throttled. Printed for every
	// backend that meters traffic, zeros included — "0 request(s)" is an answer.
	if res.Metered {
		fmt.Printf("okfpub: %d request(s), %d retried after 429, %d after 5xx\n",
			res.Stats.Requests, res.Stats.Throttled, res.Stats.Transient)
	}
	// Reclaimed rows are worth naming: each one is a row an earlier run created and
	// died before recording, so a run that keeps reporting them is reporting that
	// runs keep dying partway (#135).
	if res.Reclaimed > 0 {
		fmt.Printf("okfpub: reclaimed %d unrecorded row(s) left by an earlier interrupted run\n", res.Reclaimed)
	}
	return nil
}

// prefixLabel renders a resolved source prefix for the run log, naming the
// repo-root case explicitly so an empty prefix reads as a resolved answer rather
// than as missing output.
func prefixLabel(prefix string) string {
	if prefix == "" {
		return "repo root"
	}
	return prefix + "/"
}

// gitIn returns a source.Git that runs git subcommands in dir — the local-git
// fallback tier of source resolution. A failing subcommand (not a repo, no origin)
// returns its error, which source.Resolve treats as "this tier is unavailable".
func gitIn(dir string) source.Git {
	return func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.Output()
		return string(out), err // untrimmed, per the source.Git contract
	}
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
  run      Publish the bundle to a backend (--backend notion|fake|fs).
  version  Print the version.

Run flags:
  --backend  notion|fake|fs         (default notion)
  --bundle   bundle root            (default ".")
  --config   okf.toml path          (default: discovered)
  --areas    areas.json path        (default: <root>/areas.json if present)
  --schema   schema.json path       (default: <root>/schema.json if present)
  --out      fs/export output dir   (default: okfpub-export)
  --dry-run  export to the filesystem instead of publishing (implies --backend fs)
  --recompute                       full live-block scan (true drift + self-heal)
  --interval  minimum spacing between Notion writes (default 350ms; 0 or less disables)

Environment:
  NOTION_TOKEN    Notion integration token (required by the notion backend)
  NOTION_DB_ID    Notion data-source id    (required by the notion backend)
  OKF_SOURCE_URL  Repo web base for the generated-page banner deep-link
                  (default: GITHUB_SERVER_URL/GITHUB_REPOSITORY, else local git)
  OKF_SOURCE_REF  Branch the banner /edit/ link targets (default: git branch, else main)

Note: under the 2025-09-03 API a database id and its data-source id differ (a
database can host several data sources), and NOTION_DB_ID must be the *data-source*
id — not the database id from a Notion URL. Resolve a database id to its data
source with GET /v1/databases/{id} and read .data_sources[].
`)
}
