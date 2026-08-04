package command

import (
	"os"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/fix"
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
	fixFlag := fs.Bool("fix", false, "apply autofixable rules in place")
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

	if *fixFlag {
		fixes := fix.Enabled(b, selected, ignored)
		if fixes.Any() {
			if _, err := fix.Apply(b, fixes); err != nil {
				return 1, err
			}
			if b, err = bundle.Load(b.Root, b.Config.Path); err != nil {
				return 1, err
			}
		}
	}

	ctx := &rules.Context{Bundle: b, Config: b.Config}
	// Only pay for the qmd analysis (model load + all-pairs similarity) when a
	// qmd-backed rule will actually run — so `--ignore OKFEXT-QMD-01,OKFEXT-QMD-02` (or config
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
