package convention

import (
	"strings"
	"time"
)

// timestampLayouts are the forms a frontmatter timestamp may be written in and
// still be understood well enough for fmt to normalize it. One list, so "lint
// says this is fixable" and "fmt can actually parse it" cannot drift apart.
//
// time.RFC3339 is "2006-01-02T15:04:05Z07:00"; spelling both is redundant, not
// an additional form.
var timestampLayouts = []string{
	"2006-01-02",
	time.RFC3339,
	time.RFC3339Nano,
	"2006-01-02T15:04:05",
}

// ParseTimestamp interprets a literal frontmatter timestamp in any accepted
// layout. The bool reports whether it was understood — which is exactly the
// question "is this autofixable".
func ParseTimestamp(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	for _, layout := range timestampLayouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// TimestampMatches reports whether a literal timestamp is already written in
// the configured format. An unrecognized format matches everything, so an
// unknown config value never produces findings.
func TimestampMatches(val, format string) bool {
	val = strings.TrimSpace(val)
	switch format {
	case "date":
		_, err := time.Parse("2006-01-02", val)
		return err == nil && len(val) == 10
	case "rfc3339":
		if _, err := time.Parse(time.RFC3339, val); err == nil {
			return true
		}
		_, err := time.Parse(time.RFC3339Nano, val)
		return err == nil
	}
	return true
}

// FormatTimestamp renders t in the configured format. It reports false for a
// format it does not know, so a caller never writes back a guess.
func FormatTimestamp(t time.Time, format string) (string, bool) {
	switch format {
	case "date":
		return t.Format("2006-01-02"), true
	case "rfc3339":
		return t.Format(time.RFC3339), true
	}
	return "", false
}
