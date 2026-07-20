package rules

import (
	"fmt"

	"github.com/sigma/okf-tools/internal/config"
)

// qmd-backed extension (OKFEXT-QMD-*). A built-in extension, not part of the OKF
// spec, so it lives in the OKFEXT namespace rather than the OKF2xx worklist band.
// The rules need a fresh qmd index and are off unless the bundle sets
// qmd.enabled. The qmd analysis itself is run by the command layer and handed in
// via Context.QMD.

func init() {
	register(&Rule{
		ID: "OKFEXT-QMD-01", Name: "near-duplicate", Category: Extension,
		Default:   Info,
		Enabled:   func(c *config.Config) bool { return c.QMD.Enabled },
		SevConfig: func(c *config.Config) string { return c.QMD.NearDuplicates },
		Check:     checkQMDNearDuplicate,
	})
	register(&Rule{
		ID: "OKFEXT-QMD-02", Name: "qmd-staleness", Category: Extension,
		Default:   Info,
		Enabled:   func(c *config.Config) bool { return c.QMD.Enabled },
		SevConfig: func(c *config.Config) string { return c.QMD.Staleness },
		Check:     checkQMDStaleness,
	})
}

// OKFEXT-QMD-01: concept pairs qmd reports as highly similar. The merge/keep
// decision is the agent's.
func checkQMDNearDuplicate(ctx *Context) []Finding {
	if ctx.QMD == nil || ctx.QMD.Unavailable != "" {
		return nil // OKFEXT-QMD-02 surfaces an unavailable index
	}
	var fs []Finding
	for _, p := range ctx.QMD.NearDup {
		fs = append(fs, Finding{Path: p.A, Line: 0,
			Message: fmt.Sprintf("near-duplicate of '%s' (qmd similarity %.2f)", p.B, p.Score)})
	}
	return fs
}

// OKFEXT-QMD-02: the qmd index is unavailable or stale — semantic recall (and
// OKFEXT-QMD-01) can't be trusted until it is refreshed.
func checkQMDStaleness(ctx *Context) []Finding {
	if ctx.QMD == nil {
		return nil
	}
	msg := ctx.QMD.Unavailable
	if msg == "" {
		msg = ctx.QMD.StaleReason
	}
	if msg == "" {
		return nil
	}
	return []Finding{{Path: ".", Line: 0, Message: msg}}
}
