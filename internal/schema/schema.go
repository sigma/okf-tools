// Package schema loads a bundle's /schema.json — the closed, authoritative
// declaration of the columns of a unified export database (the sigma/ideas
// Notion mirror, issue #117). Two consumers share this one file: the sync
// pipeline provisions and populates the database from it, and okftool lints
// mirrored-file frontmatter against it (OKFEXT-SCHEMA-01).
//
// A column is `source: derived` (pipeline-computed — a page's Name/path/type and
// the hidden change-detection hash — never authored) or `source: frontmatter`
// (authored in the file and therefore linted). The lint validates only the
// frontmatter columns; derived columns are the pipeline's business and are
// ignored. The file is a JSON object keyed by column name, so the key *is* the
// name and name uniqueness is enforced by JSON itself.
package schema

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Column is one declared column's body — the value under a column-name key. The
// name lives in the map key, not here.
type Column struct {
	// Kind is the friendly column kind: title|text|select|list|number|date|checkbox.
	Kind string `json:"kind"`
	// Source is derived (pipeline-computed) or frontmatter (authored and linted).
	Source string `json:"source"`
	// Options is the closed value vocabulary for a select/list column; nil means
	// any value of the kind is accepted.
	Options []string `json:"options"`
}

// IsFrontmatter reports whether the column is authored in a file's frontmatter
// (and so subject to the lint) rather than pipeline-derived.
func (c Column) IsFrontmatter() bool { return c.Source == "frontmatter" }

// IsDerived reports whether the column is pipeline-computed — never authored, and
// so ignored by the frontmatter lint.
func (c Column) IsDerived() bool { return c.Source == "derived" }

// Schema is a parsed /schema.json.
type Schema struct {
	// Columns maps each declared column's name to its body, verbatim from the file.
	Columns map[string]Column
	// Path is the file the schema was loaded from, for diagnostics.
	Path string
}

// Load reads and parses the schema.json at path. The file must be a JSON object
// keyed by column name; anything else is an error — the schema is authoritative,
// so a malformed one fails loudly rather than silently linting nothing.
func Load(path string) (*Schema, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cols map[string]Column
	if err := json.Unmarshal(raw, &cols); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &Schema{Columns: cols, Path: path}, nil
}

// Lookup finds a declared column by name, case-insensitively — frontmatter keys
// are case-folded when mapped to columns (the sync pipeline lowercases them), so
// the lint matches the same way.
func (s *Schema) Lookup(name string) (Column, bool) {
	name = strings.ToLower(name)
	for n, c := range s.Columns {
		if strings.ToLower(n) == name {
			return c, true
		}
	}
	return Column{}, false
}

// HasTitleColumn reports whether the schema declares a title-kind column — the
// page's built-in title (the derived Name column). It is authored via the
// conventional `title:` frontmatter key, which the lint recognizes as its source.
func (s *Schema) HasTitleColumn() bool {
	for _, c := range s.Columns {
		if c.Kind == "title" {
			return true
		}
	}
	return false
}
