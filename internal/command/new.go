package command

import (
	"fmt"
	"github.com/spf13/pflag"
	"os"
	"path/filepath"
	"strings"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/config"
)

// New scaffolds a conformant concept page (frontmatter + a Citations stub),
// preventing drift at creation.
func New(args []string) (int, error) {
	fs := pflag.NewFlagSet("new", pflag.ContinueOnError)
	var g globals
	registerGlobals(fs, &g)
	typ := fs.String("type", "", "concept type (required)")
	title := fs.String("title", "", "display title (default: derived from filename)")
	rest, code, ok := parseFlags(fs, args)
	if !ok {
		return code, nil
	}
	if len(rest) != 1 {
		return 2, fmt.Errorf("usage: okf new <path> --type <T> [--title <title>]")
	}
	if strings.TrimSpace(*typ) == "" {
		return 2, fmt.Errorf("--type is required")
	}
	target := rest[0]
	if !strings.HasSuffix(target, ".md") {
		target += ".md"
	}
	if _, err := os.Stat(target); err == nil {
		return 1, fmt.Errorf("%s already exists", target)
	}

	cfg := config.Default()
	if root, cfgPath, err := bundle.Discover(filepath.Dir(target), g.bundle, g.config); err == nil {
		if c, e := config.Load(cfgPath); e == nil {
			cfg = c
		}
		_ = root
	}

	base := filepath.Base(target)
	if cfg.Filenames.Case == "kebab" && !isKebab(base) {
		return 1, fmt.Errorf("filename %q is not kebab-case; rename or set filenames.case", base)
	}

	display := *title
	if display == "" {
		display = titleFromStem(strings.TrimSuffix(base, ".md"))
	}

	if dir := filepath.Dir(target); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 1, err
		}
	}
	content := scaffold(*typ, display)
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return 1, err
	}
	fmt.Fprintf(os.Stdout, "created %s\n", target)
	return 0, nil
}

func scaffold(typ, title string) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("type: " + typ + "\n")
	b.WriteString("title: " + title + "\n")
	b.WriteString("description:\n")
	b.WriteString("---\n\n")
	b.WriteString("# Citations\n")
	return b.String()
}

var kebabName = func(s string) bool {
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return s != "" && !strings.HasPrefix(s, "-") && !strings.HasSuffix(s, "-") && !strings.Contains(s, "--")
}

func isKebab(base string) bool { return kebabName(strings.TrimSuffix(base, ".md")) }

func titleFromStem(stem string) string {
	words := strings.FieldsFunc(stem, func(r rune) bool { return r == '-' || r == '_' })
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}
