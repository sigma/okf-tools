// Package rules is the catalog of okf lint rules (docs/RULES.md). Each rule is
// a pure function over an in-memory bundle, registered against a stable ID. The
// runner resolves each rule's effective severity from config + CLI selection,
// then stamps it onto the findings the rule produces.
package rules

import (
	"sort"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/config"
	"github.com/sigma/okf-tools/internal/qmd"
)

// Severity ranks a finding. The zero value is Off (disabled).
type Severity int

const (
	Off Severity = iota
	Info
	Warning
	Error
)

func (s Severity) String() string {
	switch s {
	case Info:
		return "info"
	case Warning:
		return "warning"
	case Error:
		return "error"
	default:
		return "off"
	}
}

// MarshalJSON renders a severity as its lowercase string.
func (s Severity) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// ParseSeverity parses "off"/"info"/"warning"/"error" (case-sensitive).
func ParseSeverity(s string) (Severity, bool) {
	switch s {
	case "off":
		return Off, true
	case "info":
		return Info, true
	case "warning":
		return Warning, true
	case "error":
		return Error, true
	}
	return Off, false
}

// Category is a rule's posture group.
type Category int

const (
	Conformance Category = iota // OKF0xx: always on, fixed at error
	Policy                      // OKF1xx: configurable
	Worklist                    // OKF2xx: advisory, hard-capped at info
	Extension                   // OKFEXT-<EXT>-<NN>: non-spec, opt-in; configurable like Policy
)

// Finding is a single reported problem.
type Finding struct {
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	Path     string   `json:"path"` // bundle-relative
	Line     int      `json:"line"`
	Message  string   `json:"message"`
	Fixable  bool     `json:"fixable"`
}

// Context is what a rule's Check runs against.
type Context struct {
	Bundle *bundle.Bundle
	Config *config.Config
	QMD    *qmd.Result // qmd analysis for OKFEXT-QMD-*; nil when qmd.enabled is off
}

// Rule is one catalog entry.
type Rule struct {
	ID          string
	Name        string
	Category    Category
	Default     Severity
	Fixable     bool
	HardCapInfo bool // severity can never exceed Info (OKF202 and all worklist)

	// Enabled reports whether config turns this rule on. Nil means always on.
	Enabled func(*config.Config) bool
	// SevConfig optionally supplies a config-provided severity string that
	// overrides Default (e.g. filenames.severity for OKF103). "" means none.
	SevConfig func(*config.Config) string
	// Check produces findings; the runner stamps their severity.
	Check func(*Context) []Finding
}

var (
	registry = map[string]*Rule{}
	order    []string
)

func register(r *Rule) {
	if _, dup := registry[r.ID]; dup {
		panic("duplicate rule " + r.ID)
	}
	registry[r.ID] = r
	order = append(order, r.ID)
}

// All returns every registered rule, sorted by ID.
func All() []*Rule {
	ids := append([]string(nil), order...)
	sort.Strings(ids)
	out := make([]*Rule, len(ids))
	for i, id := range ids {
		out[i] = registry[id]
	}
	return out
}

// Get returns a rule by ID, or nil.
func Get(id string) *Rule { return registry[id] }

// Effective resolves a rule's severity for the given config, returning Off when
// the rule is disabled. Conformance rules are fixed at Error and ignore config;
// hard-capped rules can never exceed Info.
func Effective(r *Rule, cfg *config.Config) Severity {
	if r.Enabled != nil && !r.Enabled(cfg) {
		return Off
	}
	if r.Category == Conformance {
		return Error
	}
	sev := r.Default
	if r.SevConfig != nil {
		if s, ok := ParseSeverity(r.SevConfig(cfg)); ok {
			sev = s
		}
	}
	if v, ok := cfg.Rules[r.ID]; ok {
		if s, ok := ParseSeverity(v); ok {
			sev = s
		}
	}
	if r.HardCapInfo && sev > Info {
		sev = Info
	}
	return sev
}

// Run executes every rule not filtered out by selected/ignored, stamping each
// finding with its rule's effective severity. Findings are sorted by path,
// line, then rule. selected empty means "all rules".
func Run(ctx *Context, selected, ignored map[string]bool) []Finding {
	var out []Finding
	for _, r := range All() {
		if len(selected) > 0 && !selected[r.ID] {
			continue
		}
		if ignored[r.ID] {
			continue
		}
		sev := Effective(r, ctx.Config)
		if sev == Off {
			continue
		}
		for _, f := range r.Check(ctx) {
			f.Rule = r.ID
			f.Severity = sev
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Rule < out[j].Rule
	})
	return out
}

// qmdBackedRules need ctx.QMD (the qmd analysis); kept in sync with docs/RULES.md.
var qmdBackedRules = []string{"OKFEXT-QMD-01", "OKFEXT-QMD-02"}

// NeedsQMD reports whether any qmd-backed rule will actually run under cfg and the
// given CLI selection. The command layer uses it to skip the expensive qmd
// analysis when those rules are disabled, deselected, or ignored (e.g.
// `--ignore OKFEXT-QMD-01,OKFEXT-QMD-02`), so a routine lint pays no qmd cost.
func NeedsQMD(cfg *config.Config, selected, ignored map[string]bool) bool {
	for _, id := range qmdBackedRules {
		if len(selected) > 0 && !selected[id] {
			continue
		}
		if ignored[id] {
			continue
		}
		if r := Get(id); r != nil && Effective(r, cfg) != Off {
			return true
		}
	}
	return false
}

// Summary counts findings by severity.
type Summary struct {
	Error   int `json:"error"`
	Warning int `json:"warning"`
	Info    int `json:"info"`
}

// Summarize tallies findings by severity.
func Summarize(fs []Finding) Summary {
	var s Summary
	for _, f := range fs {
		switch f.Severity {
		case Error:
			s.Error++
		case Warning:
			s.Warning++
		case Info:
			s.Info++
		}
	}
	return s
}
