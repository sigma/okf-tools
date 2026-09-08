package rules

import (
	"regexp"
	"strings"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/convention"
	"gopkg.in/yaml.v3"
)

const maxInt = int(^uint(0) >> 1)

// fmScalar returns the literal scalar value (as written) of a top-level
// frontmatter key, using the order-preserving yaml.Node so the original text
// survives (yaml would otherwise reparse e.g. a date into a time.Time).
func fmScalar(node *yaml.Node, key string) (val string, found bool) {
	if node == nil {
		return "", false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1].Value, true
		}
	}
	return "", false
}

var kebabRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// isKebabName reports whether a filename's stem is lowercase-hyphenated.
func isKebabName(name string) bool {
	return kebabRe.MatchString(strings.TrimSuffix(name, ".md"))
}

// unambiguousWikilinkTarget returns the single concept a wikilink names, or nil
// if zero or many match. Delegates to the shared bundle resolver.
func unambiguousWikilinkTarget(b *bundle.Bundle, target string) *bundle.Doc {
	return b.ResolveWikilink(target)
}

func formatLabel(format string) string {
	switch format {
	case "date":
		return "a YYYY-MM-DD date"
	case "rfc3339":
		return "an RFC3339 datetime"
	}
	return format
}

// timestampFixable reports whether a non-conforming timestamp can be understood
// well enough for fmt to normalize it — the same parse fmt itself will perform,
// so a finding marked fixable is one the fix engine can actually repair.
func timestampFixable(val string) bool {
	_, ok := convention.ParseTimestamp(val)
	return ok
}
