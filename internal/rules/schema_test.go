package rules

import (
	"strings"
	"testing"
)

// TestSchemaConformance covers OKFEXT-SCHEMA-01: a schema-conformant page is
// silent; a page with an out-of-vocabulary select, an undeclared key, and a
// malformed date fires three findings; derived columns (type) and the title key
// are tolerated; and disabling the extension silences it entirely.
func TestSchemaConformance(t *testing.T) {
	b := loadFixture(t, "schema-01")
	fs := Run(&Context{Bundle: b, Config: b.Config}, nil, nil)

	byPath := map[string]int{}
	var messages []string
	for _, f := range fs {
		if f.Rule != "OKFEXT-SCHEMA-01" {
			continue
		}
		byPath[f.Path]++
		messages = append(messages, f.Message)
		if f.Line != 1 {
			t.Errorf("finding at line %d, want 1 (frontmatter): %s", f.Line, f.Message)
		}
	}

	// good.md is fully conformant — title feeds the derived Name, type is derived
	// (ignored), and every other key is a valid source:frontmatter column.
	if byPath["good.md"] != 0 {
		t.Errorf("good.md: OKFEXT-SCHEMA-01 = %d, want 0 (%v)", byPath["good.md"], messages)
	}
	// bad.md: the enum miss, the undeclared key, and the malformed date.
	if byPath["bad.md"] != 3 {
		t.Fatalf("bad.md: OKFEXT-SCHEMA-01 = %d, want 3 (%v)", byPath["bad.md"], messages)
	}
	wantSubstr := []string{
		`'status' = "seed" is not one of the schema options`,
		`undeclared frontmatter key 'author'`,
		`'created' must be a date`,
	}
	for _, want := range wantSubstr {
		if !anyContains(messages, want) {
			t.Errorf("missing expected finding %q in %v", want, messages)
		}
	}

	// Disabled ⇒ no findings, even though b.Schema is still loaded.
	b.Config.Schema.Enabled = false
	if got := countByRule(Run(&Context{Bundle: b, Config: b.Config}, nil, nil))["OKFEXT-SCHEMA-01"]; got != 0 {
		t.Errorf("disabled: OKFEXT-SCHEMA-01 = %d, want 0", got)
	}
}

// TestSchemaPromotable covers that a bundle can escalate the extension to a
// build-failing error via [rules], like every other extension.
func TestSchemaPromotable(t *testing.T) {
	b := loadFixture(t, "schema-01")
	b.Config.Rules["OKFEXT-SCHEMA-01"] = "error"
	var got int
	for _, f := range Run(&Context{Bundle: b, Config: b.Config}, nil, nil) {
		if f.Rule == "OKFEXT-SCHEMA-01" {
			got++
			if f.Severity != Error {
				t.Errorf("promoted finding severity = %v, want error", f.Severity)
			}
		}
	}
	if got != 3 {
		t.Errorf("promoted OKFEXT-SCHEMA-01 = %d, want 3", got)
	}
}

func anyContains(haystack []string, sub string) bool {
	for _, s := range haystack {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
