package fix

import (
	"testing"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/config"
	"github.com/sigma/okf-tools/internal/rules"
)

// Enabled generates the lint --fix set from the rule catalog, so a fixable rule
// can never be silently dropped by a forgotten hand-wired mapping (the bug an
// earlier candidate removed). Assert every enabled rule that declares a Fix is
// routed into the set.
func TestEnabledRoutesEveryFixableRule(t *testing.T) {
	cfg := config.Default()
	cfg.Links.Style = "relative" // OKF102 is enabled only under a relative/absolute style
	b := &bundle.Bundle{Config: cfg}

	set := Enabled(b, nil, nil)
	for _, r := range rules.All() {
		if r.Fix != rules.FixNone && !set.has(r.Fix) {
			t.Errorf("rule %s declares Fix %d but Enabled dropped it — a fixable rule must never silently no-op", r.ID, r.Fix)
		}
	}
}
