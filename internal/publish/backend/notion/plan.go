package notion

import (
	"fmt"
	"strings"
	"time"
)

// Plan is the Notion workspace plan a mirror lives on. Notion meters a connection
// per MINUTE — a budget spendable at any pace within the window — and the size of
// that budget depends on the plan: Business and Enterprise get 600 requests per
// minute, every other plan 180 (https://developers.notion.com/reference/request-limits).
// The plan is the one fact the client cannot discover for itself (the API does
// not expose it), so the caller states it and the pacing is derived from it
// (sigma/okf-tools#210).
type Plan string

const (
	PlanFree       Plan = "free"
	PlanPlus       Plan = "plus"
	PlanBusiness   Plan = "business"
	PlanEnterprise Plan = "enterprise"
)

// Plans lists the accepted plans, in the order a usage message names them.
var Plans = []Plan{PlanFree, PlanPlus, PlanBusiness, PlanEnterprise}

// The documented per-connection budgets, in requests per minute. These are the
// only rate figures the client knows; every interval is derived from one of them,
// DefaultInterval (an unstated plan) from the standard one.
const (
	budgetStandard = 180 // Free, Plus — "an average of 3 per second"
	budgetBusiness = 600 // Business, Enterprise — "an average of 10 per second"
)

// ParsePlan reads a plan name as a flag or environment variable carries it,
// case-insensitively. Anything but the four documented plans is an error naming
// them all, since that message is what a usage error shows.
func ParsePlan(s string) (Plan, error) {
	p := Plan(strings.ToLower(strings.TrimSpace(s)))
	for _, known := range Plans {
		if p == known {
			return p, nil
		}
	}
	names := make([]string, len(Plans))
	for i, known := range Plans {
		names[i] = string(known)
	}
	return "", fmt.Errorf("unknown Notion plan %q (want one of %s)", s, strings.Join(names, ", "))
}

// Budget reports the plan's documented request budget per minute — the bucket's
// capacity. An unrecognised plan reports the standard budget: ParsePlan is where
// a bad name is refused, and this must not turn a typo into unlimited traffic.
func (p Plan) Budget() int {
	switch p {
	case PlanBusiness, PlanEnterprise:
		return budgetBusiness
	default:
		return budgetStandard
	}
}

// Interval reports the sustained spacing the plan's budget names: one request per
// (minute / budget). It is what a full bucket refills at, and the only interval
// the client paces by unless the operator overrides it.
func (p Plan) Interval() time.Duration {
	return time.Minute / time.Duration(p.Budget())
}
