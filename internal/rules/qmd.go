package rules

import (
	"fmt"

	"github.com/sigma/okf-tools/internal/config"
)

// Category D — qmd-backed (optional). OKF203/OKF204 need a fresh qmd index and
// are off unless the bundle sets qmd.enabled. Advisory, hard-capped at info. The
// qmd analysis itself is run by the command layer and handed in via Context.QMD.

func init() {
	register(&Rule{
		ID: "OKF203", Name: "near-duplicate", Category: Worklist,
		Default: Info, HardCapInfo: true,
		Enabled:   func(c *config.Config) bool { return c.QMD.Enabled },
		SevConfig: func(c *config.Config) string { return c.QMD.NearDuplicates },
		Check:     checkOKF203,
	})
	register(&Rule{
		ID: "OKF204", Name: "qmd-staleness", Category: Worklist,
		Default: Info, HardCapInfo: true,
		Enabled:   func(c *config.Config) bool { return c.QMD.Enabled },
		SevConfig: func(c *config.Config) string { return c.QMD.Staleness },
		Check:     checkOKF204,
	})
}

// OKF203: concept pairs qmd reports as highly similar. The merge/keep decision
// is the agent's.
func checkOKF203(ctx *Context) []Finding {
	if ctx.QMD == nil || ctx.QMD.Unavailable != "" {
		return nil // OKF204 surfaces an unavailable index
	}
	var fs []Finding
	for _, p := range ctx.QMD.NearDup {
		fs = append(fs, Finding{Path: p.A, Line: 0,
			Message: fmt.Sprintf("near-duplicate of '%s' (qmd similarity %.2f)", p.B, p.Score)})
	}
	return fs
}

// OKF204: the qmd index is unavailable or stale — semantic recall (and OKF203)
// can't be trusted until it is refreshed.
func checkOKF204(ctx *Context) []Finding {
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
