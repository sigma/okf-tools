package command

import (
	_ "embed"
	"fmt"
	"os"

	"github.com/spf13/pflag"
)

//go:embed skill.md
var skillMarkdown string

// SkillMarkdown returns the bundled agent skill as a Claude Code SKILL.md.
func SkillMarkdown() string { return skillMarkdown }

// Skill prints the bundled agent skill to stdout, so a project can install it,
// e.g. `okftool skill > .claude/skills/okftool/SKILL.md`.
func Skill(args []string) (int, error) {
	fs := pflag.NewFlagSet("skill", pflag.ContinueOnError)
	if _, code, ok := parseFlags(fs, args); !ok {
		return code, nil
	}
	fmt.Fprint(os.Stdout, skillMarkdown)
	return 0, nil
}
