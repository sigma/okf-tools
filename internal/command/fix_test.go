package command

import (
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/config"
	"github.com/sigma/okf-tools/internal/rules"
)

// enabledFixes generates the lint --fix set from the rule catalog, so a fixable
// rule can never be silently dropped by a forgotten hand-wired mapping (the bug
// this candidate removed). Assert every enabled rule that declares a Fix is routed.
func TestEnabledFixesRoutesEveryFixableRule(t *testing.T) {
	cfg := config.Default()
	cfg.Links.Style = "relative" // OKF102 is enabled only under a relative/absolute style
	b := &bundle.Bundle{Config: cfg}

	fixes := enabledFixes(b, nil, nil)
	for _, r := range rules.All() {
		if r.Fix != rules.FixNone && !fixes.has(r.Fix) {
			t.Errorf("rule %s declares Fix %d but enabledFixes dropped it — a fixable rule must never silently no-op", r.ID, r.Fix)
		}
	}
}

// Finding.Fixable is per-instance output; it must never claim a repair the engine
// cannot make, so a fixable finding's rule must itself declare a Fix.
func TestFixableFindingImpliesRuleFix(t *testing.T) {
	for _, name := range []string{"okf101", "okf102", "okf104", "okf105", "okf106", "footnote"} {
		for _, f := range runFixture(t, fixtureDir(name)) {
			if !f.Fixable {
				continue
			}
			if r := rules.Get(f.Rule); r == nil || r.Fix == rules.FixNone {
				t.Errorf("fixture %s: finding for %s has Fixable=true but its rule declares no Fix", name, f.Rule)
			}
		}
	}
}
