package notion

import "strings"

// Notion's `code` block accepts a fixed enum of language strings and rejects any
// value outside it (including an empty/undefined one) with a 400 validation_error.
// notionCodeLanguage maps a Markdown fence token onto that enum, folding common
// aliases and defaulting empty or unrecognized tokens to "plain text".
//
// The mapping is data-driven: to teach it a new token, add an entry to codeAliases
// (for a synonym) or codeLanguages (for a value Notion supports verbatim) — no
// serialization logic changes.
func notionCodeLanguage(token string) string {
	t := strings.ToLower(strings.TrimSpace(token))
	if t == "" {
		return codeLanguagePlainText
	}
	if canon, ok := codeAliases[t]; ok {
		return canon
	}
	if codeLanguages[t] {
		return t
	}
	return codeLanguagePlainText
}

const codeLanguagePlainText = "plain text"

// codeLanguages is Notion's supported code-block language enum. A token that
// matches one verbatim (after normalization) is used as-is.
var codeLanguages = map[string]bool{
	"abap": true, "agda": true, "arduino": true, "ascii art": true,
	"assembly": true, "bash": true, "basic": true, "bnf": true, "c": true,
	"c#": true, "c++": true, "clojure": true, "coffeescript": true, "coq": true,
	"css": true, "dart": true, "dhall": true, "diff": true, "docker": true,
	"ebnf": true, "elixir": true, "elm": true, "erlang": true, "f#": true,
	"flow": true, "fortran": true, "gherkin": true, "glsl": true, "go": true,
	"graphql": true, "groovy": true, "haskell": true, "hcl": true, "html": true,
	"idris": true, "java": true, "javascript": true, "json": true, "julia": true,
	"kotlin": true, "latex": true, "less": true, "lisp": true, "livescript": true,
	"llvm": true, "lua": true, "makefile": true, "markdown": true, "markup": true,
	"matlab": true, "mathematica": true, "mermaid": true, "nix": true,
	"notion formula": true, "objective-c": true, "ocaml": true, "pascal": true,
	"perl": true, "php": true, codeLanguagePlainText: true, "powershell": true,
	"prolog": true, "protobuf": true, "python": true, "r": true, "racket": true,
	"reason": true, "ruby": true, "rust": true, "sass": true, "scala": true,
	"scheme": true, "scss": true, "shell": true, "solidity": true, "sql": true,
	"swift": true, "toml": true, "typescript": true, "vb.net": true,
	"verilog": true, "vhdl": true, "visual basic": true, "webassembly": true,
	"xml": true, "yaml": true,
}

// codeAliases folds common Markdown fence tokens onto their Notion enum value.
var codeAliases = map[string]string{
	"yml":           "yaml",
	"ts":            "typescript",
	"tsx":           "typescript",
	"js":            "javascript",
	"jsx":           "javascript",
	"golang":        "go",
	"sh":            "shell",
	"zsh":           "shell",
	"shell-session": "shell",
	"console":       "shell",
	"py":            "python",
	"rb":            "ruby",
	"rs":            "rust",
	"kt":            "kotlin",
	"hs":            "haskell",
	"cpp":           "c++",
	"cs":            "c#",
	"csharp":        "c#",
	"fsharp":        "f#",
	"objc":          "objective-c",
	"md":            "markdown",
	"dockerfile":    "docker",
	"proto":         "protobuf",
	"ps1":           "powershell",
	"pwsh":          "powershell",
	"tf":            "hcl",
	"terraform":     "hcl",
	"htm":           "html",
	"txt":           codeLanguagePlainText,
	"text":          codeLanguagePlainText,
	"plaintext":     codeLanguagePlainText,
	"plain":         codeLanguagePlainText,
	"none":          codeLanguagePlainText,
	"wasm":          "webassembly",
	"vbnet":         "vb.net",
	"tex":           "latex",
}
