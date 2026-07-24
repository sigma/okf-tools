package rules

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sigma/okf-tools/internal/config"
	"github.com/sigma/okf-tools/internal/schema"
)

// Frontmatter-schema extension (OKFEXT-SCHEMA-01). A built-in, non-spec extension
// gated on schema.enabled and scoped to the bundle's /schema.json — the closed,
// authoritative column declaration for an export database (sigma/ideas #117). It
// is the strict counterpart to the sync pipeline, which *silently drops* any
// frontmatter key that is not a declared column: the lint surfaces those drops
// (and value/enum violations) at author time instead. Defaults to warning so a
// bundle can promote it to a hard failure via [rules].
//
// Semantics (the closed schema, #123):
//   - a `source: frontmatter` column is validated — the value must match the
//     column's kind, and where the column carries `options`, be one of them;
//   - a `source: derived` column is IGNORED — Name/path/type/hash are computed by
//     the pipeline, never authored, so a frontmatter key naming one is skipped,
//     neither required nor rejected;
//   - the conventional `title:` key is tolerated when the schema declares a
//     title-kind column (the derived Name it feeds);
//   - every other key is undeclared, and flagged.

func init() {
	register(&Rule{
		ID: "OKFEXT-SCHEMA-01", Name: "frontmatter-schema-conformance", Category: Extension,
		Default: Warning,
		Enabled: func(c *config.Config) bool { return c.Schema.Enabled },
		Check:   checkSchemaConformance,
	})
}

func checkSchemaConformance(ctx *Context) []Finding {
	s := ctx.Bundle.Schema
	if s == nil { // enabled but no schema loaded — nothing to validate against
		return nil
	}
	schemaName := filepath.Base(s.Path)
	hasTitle := s.HasTitleColumn()

	var fs []Finding
	for _, d := range ctx.Bundle.Concepts {
		if !d.HasFrontmatter() {
			continue // OKF001 owns parse-error reporting; nothing to validate here
		}
		// Deterministic order so findings sort stably across runs (the map has none).
		keys := make([]string, 0, len(d.Frontmatter))
		for k := range d.Frontmatter {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, k := range keys {
			col, declared := s.Lookup(k)
			switch {
			case declared && col.IsDerived():
				// Pipeline-owned (Name/path/type/hash) — ignore entirely.
			case declared: // a source:frontmatter column — validate value + options
				if msg := validateSchemaValue(k, d.Frontmatter[k], col); msg != "" {
					fs = append(fs, Finding{Path: d.Rel, Line: 1, Message: msg})
				}
			case strings.EqualFold(k, "title") && hasTitle:
				// The built-in page title, authored via `title:`; feeds the derived
				// title-kind column, so it is a declared value, not an undeclared key.
			default:
				fs = append(fs, Finding{Path: d.Rel, Line: 1,
					Message: fmt.Sprintf("undeclared frontmatter key '%s' (not a source:frontmatter column in %s)", k, schemaName)})
			}
		}
	}
	return fs
}

// validateSchemaValue checks an authored value against its declared column's kind
// and, where present, its closed options; it returns an empty string when valid.
func validateSchemaValue(key string, val any, col schema.Column) string {
	switch col.Kind {
	case "title", "text":
		if _, ok := val.(string); !ok {
			return fmt.Sprintf("frontmatter '%s' must be text (a string) per schema", key)
		}
	case "select":
		s, ok := val.(string)
		if !ok {
			return fmt.Sprintf("frontmatter '%s' must be a select (a string) per schema", key)
		}
		if len(col.Options) > 0 && !slices.Contains(col.Options, s) {
			return fmt.Sprintf("frontmatter '%s' = %q is not one of the schema options [%s]", key, s, strings.Join(col.Options, ", "))
		}
	case "list":
		items, ok := val.([]any)
		if !ok {
			return fmt.Sprintf("frontmatter '%s' must be a list per schema", key)
		}
		if len(col.Options) > 0 {
			for _, it := range items {
				s, ok := it.(string)
				if !ok || !slices.Contains(col.Options, s) {
					return fmt.Sprintf("frontmatter '%s' item %v is not one of the schema options [%s]", key, it, strings.Join(col.Options, ", "))
				}
			}
		}
	case "number":
		switch val.(type) {
		case int, int64, uint64, float64:
		default:
			return fmt.Sprintf("frontmatter '%s' must be a number per schema", key)
		}
	case "date":
		if !isDateValue(val) {
			return fmt.Sprintf("frontmatter '%s' must be a date per schema", key)
		}
	case "checkbox":
		if _, ok := val.(bool); !ok {
			return fmt.Sprintf("frontmatter '%s' must be a checkbox (true/false) per schema", key)
		}
	}
	return ""
}

// isDateValue reports whether a decoded frontmatter value is a date: a YAML
// timestamp (decoded to time.Time) or a string in ISO date / RFC3339 form.
func isDateValue(val any) bool {
	switch v := val.(type) {
	case time.Time:
		return true
	case string:
		return matchesTimestamp(v, "date") || matchesTimestamp(v, "rfc3339")
	}
	return false
}
