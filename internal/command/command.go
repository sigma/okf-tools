// Package command implements the okf subcommands (lint, index, fmt, new, graph)
// and their shared plumbing: global flags, bundle discovery/loading, and the
// human/JSON renderers. Each command returns an exit code and an error.
package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/rules"
	"github.com/spf13/pflag"
)

// globals are the flags every subcommand accepts.
type globals struct {
	bundle string
	config string
	format string
}

// parseFlags parses args and returns the positional arguments. pflag interleaves
// flags and positionals natively, so no manual reordering is needed. ok is false
// when the caller should return the given code without doing work (parse error →
// 2, -h/--help → 0).
func parseFlags(fs *pflag.FlagSet, args []string) (positionals []string, code int, ok bool) {
	err := fs.Parse(args)
	switch {
	case err == nil:
		return fs.Args(), 0, true
	case errors.Is(err, pflag.ErrHelp):
		return nil, 0, false
	default:
		fmt.Fprintln(os.Stderr, "okftool: "+err.Error())
		return nil, 2, false
	}
}

func registerGlobals(fs *pflag.FlagSet, g *globals) {
	fs.StringVar(&g.bundle, "bundle", "", "bundle root directory (default: auto-discover)")
	fs.StringVar(&g.config, "config", "", "config file (default: okf.toml at bundle root)")
	fs.StringVar(&g.format, "format", "human", "output format: human|json")
}

// loadBundle discovers and loads the bundle, seeding discovery from the first
// path argument when --bundle is not given.
func loadBundle(g *globals, paths []string) (*bundle.Bundle, error) {
	root, cfgPath, err := bundle.Discover(discoverStart(paths), g.bundle, g.config)
	if err != nil {
		return nil, err
	}
	return bundle.Load(root, cfgPath)
}

func discoverStart(paths []string) string {
	if len(paths) == 0 {
		return "."
	}
	p := paths[0]
	if info, err := os.Stat(p); err == nil && !info.IsDir() {
		return filepath.Dir(p)
	}
	return p
}

// parseRuleSet turns a comma-separated list of rule IDs into a set (uppercased).
func parseRuleSet(csv string) map[string]bool {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	set := map[string]bool{}
	for _, part := range strings.Split(csv, ",") {
		if id := strings.ToUpper(strings.TrimSpace(part)); id != "" {
			set[id] = true
		}
	}
	return set
}

// jsonEnvelope is the stable machine-readable output (docs/DESIGN.md §Output).
type jsonEnvelope struct {
	Bundle     string          `json:"bundle"`
	OKFVersion string          `json:"okf_version"`
	Summary    rules.Summary   `json:"summary"`
	Findings   []rules.Finding `json:"findings"`
}

func renderFindings(w io.Writer, format string, b *bundle.Bundle, fs []rules.Finding) error {
	switch format {
	case "json":
		env := jsonEnvelope{
			Bundle:     filepath.Base(b.Root),
			OKFVersion: b.OKFVersion,
			Summary:    rules.Summarize(fs),
			Findings:   fs,
		}
		if env.Findings == nil {
			env.Findings = []rules.Finding{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(env)
	default:
		for _, f := range fs {
			loc := f.Path
			if f.Line > 0 {
				loc = fmt.Sprintf("%s:%d", f.Path, f.Line)
			}
			fmt.Fprintf(w, "%-7s  %-40s  %s  %s\n", f.Severity, loc, f.Rule, f.Message)
		}
		s := rules.Summarize(fs)
		if len(fs) > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%d error, %d warning, %d info\n", s.Error, s.Warning, s.Info)
		return nil
	}
}

// filterByPaths keeps only findings whose path is at or under one of paths
// (converted to bundle-relative). An empty paths keeps everything.
func filterByPaths(fs []rules.Finding, b *bundle.Bundle, paths []string) []rules.Finding {
	if len(paths) == 0 {
		return fs
	}
	var rels []string
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		rel := b.Rel(abs)
		if rel == "." {
			return fs // a path equal to the bundle root matches everything
		}
		rels = append(rels, rel)
	}
	var out []rules.Finding
	for _, f := range fs {
		for _, r := range rels {
			if f.Path == r || strings.HasPrefix(f.Path, r+"/") {
				out = append(out, f)
				break
			}
		}
	}
	return out
}
