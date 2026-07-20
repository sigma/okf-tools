package rules

import "testing"

// countByRule tallies findings per rule ID.
func countByRule(fs []Finding) map[string]int {
	m := map[string]int{}
	for _, f := range fs {
		m[f.Rule]++
	}
	return m
}

// TestGlossaryStructure covers OKFEXT-GLOSSARY-01: a clean glossary is silent,
// a malformed entry is flagged at its line, and disabling the extension silences
// it entirely (scoping to declared glossary files is inherent — the rule only
// walks b.Glossaries).
func TestGlossaryStructure(t *testing.T) {
	clean := loadFixture(t, "glossary-clean")
	if got := countByRule(Run(&Context{Bundle: clean, Config: clean.Config}, nil, nil))["OKFEXT-GLOSSARY-01"]; got != 0 {
		t.Errorf("clean glossary: OKFEXT-GLOSSARY-01 = %d, want 0", got)
	}

	bad := loadFixture(t, "glossary-01")
	fs := Run(&Context{Bundle: bad, Config: bad.Config}, nil, nil)
	if got := countByRule(fs)["OKFEXT-GLOSSARY-01"]; got != 1 {
		t.Fatalf("malformed glossary: OKFEXT-GLOSSARY-01 = %d, want 1", got)
	}
	for _, f := range fs {
		if f.Rule == "OKFEXT-GLOSSARY-01" && f.Line != 4 {
			t.Errorf("finding at line %d, want 4 (the bare bullet)", f.Line)
		}
	}

	// Disabled ⇒ no findings, even on the malformed fixture.
	bad.Config.Glossary.Enabled = false
	if got := countByRule(Run(&Context{Bundle: bad, Config: bad.Config}, nil, nil))["OKFEXT-GLOSSARY-01"]; got != 0 {
		t.Errorf("disabled: OKFEXT-GLOSSARY-01 = %d, want 0", got)
	}
}
