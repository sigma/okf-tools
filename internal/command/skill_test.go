package command

import (
	"strings"
	"testing"
)

func TestSkillMarkdown(t *testing.T) {
	s := SkillMarkdown()
	if !strings.HasPrefix(s, "---\n") {
		t.Fatal("skill should start with YAML frontmatter")
	}
	for _, want := range []string{"name: okftool", "description:", "okftool lint", "OKF0xx"} {
		if !strings.Contains(s, want) {
			t.Errorf("skill markdown missing %q", want)
		}
	}
}
