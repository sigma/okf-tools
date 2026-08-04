package rules

// FixKind names the mechanical transform that repairs a rule's findings. It is the
// single source of truth for the rule→fix association: a rule declares its FixKind
// (Rule.Fix), the lint --fix path enables a rule's fix by reading it, and the
// transform engine (internal/command) keys its transforms off it — so adding a
// fixable rule is one declaration, not three hand-kept lists. FixNone (the zero
// value) means the rule has no autofix.
//
// FixFrontmatter has no owning rule: canonical frontmatter key order is applied
// only by `okftool fmt`, never by an OKFxxx rule. It is a valid FixKind that no
// Rule.Fix names, so the transform engine can enumerate it while the rule catalog
// does not.
type FixKind int

const (
	FixNone        FixKind = iota
	FixWikilinks           // OKF101: [[x]] -> [x](x.md) when unambiguous
	FixLinkStyle           // OKF102: restyle concept cross-links
	FixTimestamp           // OKF104: normalize frontmatter timestamp
	FixCitations           // OKF105: renumber citation entries
	FixIndex               // OKF106: regenerate index.md files
	FixFrontmatter         // fmt only: canonical frontmatter key order (no rule)
)
