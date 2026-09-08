// Package publishcmd implements the okfpub subcommands and their shared
// plumbing: flag parsing, the config contract (areas.json, schema.json, the
// credentials), backend selection, the fan-out, and the run's own reporting.
//
// It exists for the reason internal/command exists for okftool: a command whose
// body sits in package main is reachable only by running the binary, so its flag
// parsing, its policy and its exit code go untested. Everything here takes an
// io.Writer and returns an exit code, which makes the entrypoint the test surface
// rather than a place tests cannot go.
package publishcmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/backend/notion"
	"github.com/sigma/okf-tools/internal/publish/graph"
	"github.com/sigma/okf-tools/internal/publish/pipeline"
	"github.com/sigma/okf-tools/internal/publish/source"
)

// Run is the minimal `okfpub run` command surface: it resolves the config
// contract (areas.json, schema.json, NOTION_TOKEN, NOTION_DB_ID), selects a
// backend, and drives Generation → Optimization → Transport for one publish. The
// flag surface is deliberately minimal — just enough to select a backend, point at
// the config files, and read the credentials; the full flag design is downstream
// (sigma/ideas#172, "Out of Scope").
func Run(out io.Writer, args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(out)
	backendName := fs.String("backend", string(pipeline.BackendNotion), "publishing backend: notion|gdocs|fake|fs")
	var selections stringList
	fs.Var(&selections, "select", "publish only this area or path as one document; repeatable (gdocs). Omitted: one document per area")
	bundleDir := fs.String("bundle", ".", "bundle root (or a dir to search upward from)")
	configPath := fs.String("config", "", "okf.toml path (default: discovered)")
	areasPath := fs.String("areas", "", "areas.json path (default: <root>/areas.json if present)")
	schemaPath := fs.String("schema", "", "schema.json path (default: <root>/schema.json if present)")
	outDir := fs.String("out", "", "output dir for the fs/export backend (default: "+pipeline.DefaultOutDir+")")
	dryRun := fs.Bool("dry-run", false, "publish nothing: with --backend gdocs, dump the API writes that would be issued; otherwise export to the filesystem (implies --backend fs)")
	recompute := fs.Bool("recompute", false, "opt into the full live-block scan (true drift + subpage/anchor self-heal); default is the cheap steady-state scan. Notion only: the gdocs scan reads the whole document either way, and self-heals unconditionally")
	force := fs.Bool("force", false, "re-publish every page whatever change detection says. The escape hatch when a rendering fix cannot reach an already-published mirror; costs a full rewrite of the destination")
	interval := fs.Duration("interval", notion.DefaultInterval, "minimum spacing between Notion writes (reads burst ahead of it); zero or less disables pacing")
	if err := fs.Parse(args); err != nil {
		// -h/--help is a request, not a failure: flag has already written the usage
		// to out, so there is nothing to add and nothing to fail.
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}

	// Reject an impossible flag COMBINATION before doing any work. --select is a
	// fan-out flag and only the document backend fans out; accepting it elsewhere
	// would publish the whole bundle while the operator believed they had narrowed
	// it. Checked here rather than at the fan-out branch so the message a user gets
	// is about their flags, not about whatever the run happened to fail on first
	// (banner resolution, credentials) on the way there.
	if pipeline.BackendKind(*backendName) != pipeline.BackendGDocs && len(selections) > 0 {
		return 2, fmt.Errorf("--select applies to the %s backend only", pipeline.BackendGDocs)
	}

	// Discover + load the bundle through the shared front-end (same parser lint
	// uses); Discover resolves the root that defaults the config-file paths.
	root, cfgPath, err := bundle.Discover(*bundleDir, *bundleDir, *configPath)
	if err != nil {
		return 1, err
	}
	b, err := bundle.Load(root, cfgPath)
	if err != nil {
		return 1, err
	}

	// Resolve the config surface. areas.json / schema.json default to the bundle
	// root but are optional; the credentials come from --* args or the environment.
	cfg, err := pipeline.LoadConfig(pipeline.LoadOptions{
		AreasPath:  defaultPath(*areasPath, b.Root, "areas.json"),
		SchemaPath: defaultPath(*schemaPath, b.Root, "schema.json"),
	})
	if err != nil {
		return 1, err
	}
	cfg.OutDir = *outDir
	cfg.NotionInterval = interval

	ctx := context.Background()

	// --dry-run means "publish nothing", which two backends express differently. For
	// a document destination the interesting question is "did I build the right
	// batchUpdate", which an exported Markdown tree cannot answer — so the writes are
	// dumped instead, and Provision goes find-only so no empty document is left
	// behind. Every other backend keeps the original meaning: export to the
	// filesystem rather than touch a live workspace.
	kind := pipeline.BackendKind(*backendName)
	if *dryRun {
		if kind == pipeline.BackendGDocs {
			cfg.GDocsDryRun = out
		} else {
			kind = pipeline.BackendFS
		}
	}
	be, err := pipeline.SelectBackend(ctx, kind, cfg, filepath.Base(b.Root))
	if err != nil {
		return 1, err
	}
	// Echo a non-default pacing choice, but only where it applies: a run that is
	// unusually fast or slow should say so in its own log rather than leave the
	// reader to guess at the operator's flags. The fs/fake backends pace nothing, so
	// printing it there would describe a knob that did not turn.
	if kind == pipeline.BackendNotion && *interval != notion.DefaultInterval {
		fmt.Fprintf(out, "okfpub: notion pacing: %v between writes\n", *interval)
	}

	// Echo the resolved config surface so a scheduled run's log shows what contract
	// it published against — and so a mis-pointed --areas/--schema is visible.
	if host, ok := cfg.GlossaryFile(); ok {
		fmt.Fprintf(out, "okfpub: glossary/anchor host: %s (areas.json role marker)\n", host)
	}
	if cfg.Schema != nil {
		fmt.Fprintf(out, "okfpub: schema: %d column(s)\n", len(cfg.Schema.Columns))
	}

	var runOpts []pipeline.Option
	if *force {
		// Named in the output, because a full rewrite is what it costs and a run that
		// rewrote everything should say why it did.
		fmt.Fprintln(out, "okfpub: --force: re-publishing every page, skipping change detection")
		runOpts = append(runOpts, pipeline.WithForceRewrite())
	}
	if *recompute {
		// A flag that silently does nothing is worse than one that errors (#183).
		// Erroring would break every config that passes it for both backends, so it
		// says what it did instead — and on gdocs what it did is nothing, because
		// that scan reads the whole document and self-heals either way.
		if kind == pipeline.BackendGDocs {
			fmt.Fprintln(out, "okfpub: --recompute: no effect on the gdocs backend; its scan reads the whole document and self-heals on every run")
		}
		runOpts = append(runOpts, pipeline.WithScanMode(backend.ScanRecompute))
	}

	// Resolve the generated-page disclaimer banner (ADR-0015). Resolution reads the
	// environment and, as a fallback, local git — the bin's job, kept out of the
	// pure planner — and is threaded into Generation as data. A misconfigured source
	// URL fails loud rather than publishing a banner with a dangling deep-link.
	if bn := b.Config.Banner; bn.Enabled {
		src, err := source.Resolve(os.Getenv, gitIn(b.Root))
		if err != nil {
			return 1, err
		}
		runOpts = append(runOpts, pipeline.WithBanner(&graph.Banner{
			Text:    bn.Text,
			BaseURL: src.BaseURL,
			Ref:     src.Ref,
			Prefix:  src.Prefix,
		}))
		// Echo the prefix too: a bundle nested in its repo emits links that are only
		// correct with it, so a run that silently resolved none is worth seeing.
		fmt.Fprintf(out, "okfpub: banner: source %s @ %s (bundle at %s)\n",
			src.BaseURL, src.Ref, prefixLabel(src.Prefix))
	}

	// Every backend except gdocs publishes the whole bundle as one destination, so
	// they run once with no selection. Only the document backend fans out.
	if kind != pipeline.BackendGDocs {
		return runOnce(ctx, out, be, b, kind, runOpts)
	}
	return runFanOut(ctx, out, cfg, b, kind, selections, runOpts)
}

// runOnce publishes one destination and prints its summary.
func runOnce(ctx context.Context, out io.Writer, be backend.Backend, b *bundle.Bundle, kind pipeline.BackendKind, runOpts []pipeline.Option) (int, error) {
	prog := newProgress(out, "")
	stop := prog.start()
	res, err := pipeline.Run(ctx, be, b, append(runOpts, pipeline.WithProgress(prog.report))...)
	// Stopped BEFORE the summary prints, so no progress line lands after it.
	stop()
	if err != nil {
		return 1, err
	}
	printResult(out, kind, "", res)
	return 0, nil
}

// runFanOut publishes each selection as its own document.
//
// It is BEST-EFFORT, not fail-fast: one broken area must not stop four healthy
// ones from updating, and all-or-nothing is not available anyway since Docs has
// no cross-file transaction. A failed document simply leaves its own state
// unupdated; the run exits non-zero with a per-selection summary (#151).
func runFanOut(ctx context.Context, out io.Writer, cfg *pipeline.Config, b *bundle.Bundle, kind pipeline.BackendKind, requested []string, runOpts []pipeline.Option) (int, error) {
	plan, err := pipeline.ResolveSelections(b, requested)
	if err != nil {
		return 1, err
	}
	for _, w := range plan.Warnings {
		fmt.Fprintf(out, "okfpub: warning: %s\n", w)
	}
	if plan.Unclaimed > 0 {
		fmt.Fprintf(out, "okfpub: %d publishable page(s) belong to no declared area and were not published\n",
			plan.Unclaimed)
	}
	if len(plan.Selections) == 0 {
		return 2, fmt.Errorf("no selection to publish")
	}

	var failed int
	for _, sel := range plan.Selections {
		cfg.GDocsSelection = sel.Name
		be, err := pipeline.SelectBackend(ctx, kind, cfg, filepath.Base(b.Root))
		if err != nil {
			return 1, err
		}
		// Progress is labelled per selection: a fan-out publishes one document at a
		// time, but "3/12" with no name says which document only by luck of order.
		prog := newProgress(out, sel.Name)
		stop := prog.start()
		opts := append(append([]pipeline.Option(nil), runOpts...),
			pipeline.WithSelection(sel.Contains),
			pipeline.WithProgress(prog.report))
		res, err := pipeline.Run(ctx, be, b, opts...)
		stop()
		if err != nil {
			failed++
			fmt.Fprintf(out, "okfpub: %s: FAILED: %v\n", sel.Name, err)
			continue
		}
		printResult(out, kind, sel.Name, res)
	}
	if failed > 0 {
		return 1, fmt.Errorf("%d of %d selection(s) failed", failed, len(plan.Selections))
	}
	return 0, nil
}

// progressInterval is how often a running drain reports itself. Long enough that
// a healthy publish adds a handful of lines to a CI log rather than one per
// transaction, short enough that a wedged one is obvious while someone is
// watching (#184).
const progressInterval = 15 * time.Second

// progress reports a drain as it runs. Between the configuration banner and the
// final summary a publish is otherwise SILENT, which is how a run wedged on a
// stalled request went 34 minutes without anyone being able to tell it from a
// slow one (#184).
//
// The line is printed by a TICKER rather than by the drain: a run that is stuck
// completes no transaction, so a reporter driven by completions alone says
// nothing at exactly the moment there is something to say. Ticking republishes
// the same count instead, and a count that does not move is the signal.
type progress struct {
	label string
	// out is where the lines go. A field rather than os.Stdout inline so the
	// reporter is testable without capturing the process's own output.
	out io.Writer

	mu    sync.Mutex
	done  int
	total int
}

func newProgress(out io.Writer, selection string) *progress {
	label := ""
	if selection != "" {
		label = selection + ": "
	}
	return &progress{label: label, out: out}
}

// report is the pipeline callback: it records what has landed and prints the
// final line itself, so the counter always ends at n/n rather than wherever the
// last tick left it. It is called from the drain loop, so it does no work beyond
// recording and, once, a print.
func (p *progress) report(done, total int) {
	p.mu.Lock()
	p.done, p.total = done, total
	last := done >= total
	p.mu.Unlock()
	if last {
		p.print()
	}
}

// start begins ticking and returns the function that stops it. The stop is
// synchronous, so no line lands after the run's summary.
func (p *progress) start() func() {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(progressInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				p.print()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-stopped
		})
	}
}

// print reports the count as it stands. Nothing is printed before the drain has
// executed anything: a run still scanning has no transactions to count, and
// "0/0" would claim it does.
func (p *progress) print() {
	p.mu.Lock()
	done, total := p.done, p.total
	p.mu.Unlock()
	if total == 0 {
		return
	}
	fmt.Fprintf(p.out, "okfpub: %s%d/%d transaction(s)\n", p.label, done, total)
}

// printResult renders one destination's summary, labelled by selection when a run
// published more than one.
func printResult(out io.Writer, kind pipeline.BackendKind, selection string, res *pipeline.Result) {
	label := ""
	if selection != "" {
		label = selection + ": "
	}
	fmt.Fprintf(out, "okfpub: %spublished %d node(s), %d anchor(s) in %d transaction(s) via %s backend\n",
		label, len(res.Nodes), len(res.Anchors), res.TxnCount, kind)
	// The traffic line is what makes a slow run diagnosable without a debugger: a
	// run whose request count dwarfs its transaction count is doing per-request work
	// nobody planned, and one whose retries dominate is throttled. Printed for every
	// backend that meters traffic, zeros included — "0 request(s)" is an answer.
	if res.Metered {
		fmt.Fprintf(out, "okfpub: %d request(s), %d retried after 429, %d after a transient failure\n",
			res.Stats.Requests, res.Stats.Throttled, res.Stats.Transient)
	}
	// Reclaimed rows are worth naming: each one is a row an earlier run created and
	// died before recording, so a run that keeps reporting them is reporting that
	// runs keep dying partway (#135).
	if res.Reclaimed > 0 {
		fmt.Fprintf(out, "okfpub: reclaimed %d unrecorded row(s) left by an earlier interrupted run\n", res.Reclaimed)
	}
}

// stringList collects a repeatable string flag. okfpub uses the standard flag
// package, which has no repeatable-string type of its own.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
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

// Usage writes okfpub's help text.
func Usage(w io.Writer) {
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
  --dry-run  publish nothing. With --backend gdocs, dump the API writes that
             would be issued; otherwise export to the filesystem (--backend fs)
  --recompute                       full live-block scan (true drift + self-heal).
                                    Notion only: the gdocs scan always self-heals
  --force                           re-publish every page, skipping change
                                    detection (a full rewrite of the destination)
  --select <area|path>              publish only this area or path as one document;
                                    repeatable (gdocs). Omitted: one document per area
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
