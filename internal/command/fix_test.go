package command

import (
	"testing"

	"github.com/sigma/okf-tools/internal/rules"
)

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
