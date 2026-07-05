package command

import (
	"os"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/qmd"
	"github.com/sigma/okf-tools/internal/rules"
	"github.com/spf13/pflag"
)

// qmdConcepts adapts the bundle's concepts to the qmd package's input.
func qmdConcepts(b *bundle.Bundle) []qmd.Concept {
	out := make([]qmd.Concept, 0, len(b.Concepts))
	for _, d := range b.Concepts {
		out = append(out, qmd.Concept{Rel: d.Rel, Abs: d.Path, Text: d.Body})
	}
	return out
}

// Lint runs the rule catalog over the bundle. It is the anchor command.
func Lint(args []string) (int, error) {
	fs := pflag.NewFlagSet("lint", pflag.ContinueOnError)
	var g globals
	registerGlobals(fs, &g)
	fix := fs.Bool("fix", false, "apply autofixable rules in place")
	failOn := fs.String("fail-on", "error", "exit non-zero at this severity or above: error|warning")
	exitZero := fs.Bool("exit-zero", false, "always exit 0 (report only)")
	sel := fs.String("select", "", "only run these rules (comma-separated OKF IDs)")
	ign := fs.String("ignore", "", "skip these rules (comma-separated OKF IDs)")
	paths, code, ok := parseFlags(fs, args)
	if !ok {
		return code, nil
	}
	selected, ignored := parseRuleSet(*sel), parseRuleSet(*ign)
	if err := validateFormat(g.format, "human", "json", "sarif"); err != nil {
		return 2, err
	}

	b, err := loadBundle(&g, paths)
	if err != nil {
		return 1, err
	}

	if *fix {
		opts := lintFixOptions(b, selected, ignored)
		if opts.any() {
			if _, err := applyFixes(b, opts); err != nil {
				return 1, err
			}
			if b, err = bundle.Load(b.Root, b.Config.Path); err != nil {
				return 1, err
			}
		}
	}

	ctx := &rules.Context{Bundle: b, Config: b.Config}
	// Only pay for the qmd analysis (model load + all-pairs similarity) when a
	// qmd-backed rule will actually run — so `--ignore OKF203,OKF204` (or config
	// disabling them) makes lint truly fast, not just quiet.
	if b.Config.QMD.Enabled && rules.NeedsQMD(b.Config, selected, ignored) {
		ctx.QMD = qmd.Analyze(b.Root, qmdConcepts(b), &b.Config.QMD, nil)
	}
	findings := rules.Run(ctx, selected, ignored)
	findings = filterByPaths(findings, b, paths)
	if err := renderFindings(os.Stdout, g.format, b, findings); err != nil {
		return 1, err
	}

	if *exitZero {
		return 0, nil
	}
	threshold := rules.Error
	if *failOn == "warning" {
		threshold = rules.Warning
	}
	for _, f := range findings {
		if f.Severity >= threshold {
			return 1, nil
		}
	}
	return 0, nil
}

// lintFixOptions maps the enabled+selected fixable rules to fix transforms.
func lintFixOptions(b *bundle.Bundle, selected, ignored map[string]bool) fixOptions {
	on := func(id string) bool {
		r := rules.Get(id)
		if r == nil {
			return false
		}
		if len(selected) > 0 && !selected[id] {
			return false
		}
		if ignored[id] {
			return false
		}
		return rules.Effective(r, b.Config) != rules.Off
	}
	return fixOptions{
		Wikilinks: on("OKF101"),
		LinkStyle: on("OKF102"),
		Timestamp: on("OKF104"),
		Citations: on("OKF105"),
		Index:     on("OKF106"),
	}
}
