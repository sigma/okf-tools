package convention

import "strings"

// IsKebabFilename reports whether a page's filename is lowercase-hyphenated:
// its stem is one or more a-z0-9 words joined by single hyphens, with no
// leading, trailing or doubled hyphen. A ".md" extension is ignored.
//
// One implementation, because both halves of the naming policy have to agree:
// OKF103 reports a filename that breaks it, and `okftool new` refuses to create
// one. Written twice — a regexp in the rule catalog and a hand-rolled rune loop
// in the scaffolder — nothing but luck kept them from accepting different names,
// and the failure is one a user meets immediately: new scaffolds a page that
// lint then rejects.
func IsKebabFilename(base string) bool {
	stem := strings.TrimSuffix(base, ".md")
	if stem == "" || strings.HasPrefix(stem, "-") || strings.HasSuffix(stem, "-") || strings.Contains(stem, "--") {
		return false
	}
	for _, r := range stem {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return true
}
