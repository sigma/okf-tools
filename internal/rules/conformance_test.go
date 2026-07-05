package rules

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFrontmatterParseError(t *testing.T) {
	// A mapping value at block-relative line 2 -> file line 3 (the block opens on
	// line 1). The cleaned message drops the "yaml:"/"line N:" noise.
	var m map[string]any
	err := yaml.Unmarshal([]byte("type: Concept\n  bad: indentation"), &m)
	if err == nil {
		t.Fatal("expected a yaml error")
	}
	line, msg := frontmatterParseError(err)
	if line != 3 {
		t.Errorf("line = %d, want 3 (block line 2 -> file line 3)", line)
	}
	if strings.Contains(msg, "yaml:") || strings.Contains(msg, "line 2") {
		t.Errorf("message not cleaned: %q", msg)
	}
	if !strings.Contains(msg, "mapping values are not allowed") {
		t.Errorf("message lost its detail: %q", msg)
	}
}
