package notion

import "testing"

func TestNotionCodeLanguage(t *testing.T) {
	cases := map[string]string{
		// canonical enum values pass through
		"yaml":       "yaml",
		"json":       "json",
		"go":         "go",
		"typescript": "typescript",
		"nix":        "nix",
		"mermaid":    "mermaid",
		"shell":      "shell",
		// common aliases fold onto the enum
		"yml":        "yaml",
		"ts":         "typescript",
		"js":         "javascript",
		"sh":         "shell",
		"bash":       "bash",
		"py":         "python",
		"golang":     "go",
		"dockerfile": "docker",
		// case and surrounding whitespace are normalized
		"YAML":   "yaml",
		"  go  ": "go",
		// empty and unrecognized default to plain text (both would 400 otherwise)
		"":            "plain text",
		"not-a-lang":  "plain text",
		"brainfuck42": "plain text",
	}
	for in, want := range cases {
		if got := notionCodeLanguage(in); got != want {
			t.Errorf("notionCodeLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}
