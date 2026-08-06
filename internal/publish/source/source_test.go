package source

import "testing"

// fakeGit builds a Git that answers `remote get-url origin` and
// `rev-parse --abbrev-ref HEAD` from the given values; an empty value makes that
// subcommand fail (as if unavailable), so tests can exercise the fall-through.
// `rev-parse --show-prefix` fails, i.e. the repo-root-bundle answer — use
// fakeGitIn for a bundle nested inside its repo.
func fakeGit(remote, branch string) Git {
	return fakeGitIn(remote, branch, "")
}

// fakeGitIn extends fakeGit with a `rev-parse --show-prefix` answer: the bundle
// root's path within the repo work tree. git reports it slash-terminated (and
// empty at the repo root), which is what the fake reproduces.
func fakeGitIn(remote, branch, showPrefix string) Git {
	return func(args ...string) (string, error) {
		switch {
		case len(args) >= 2 && args[0] == "remote" && args[1] == "get-url":
			if remote == "" {
				return "", errNoGit
			}
			return remote + "\n", nil
		case len(args) >= 2 && args[0] == "rev-parse" && args[1] == "--show-prefix":
			if showPrefix == "" {
				return "", errNoGit
			}
			return showPrefix + "/\n", nil
		case len(args) >= 1 && args[0] == "rev-parse":
			if branch == "" {
				return "", errNoGit
			}
			return branch + "\n", nil
		}
		return "", errNoGit
	}
}

var errNoGit = errStr("git unavailable")

type errStr string

func (e errStr) Error() string { return string(e) }

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolvePrecedence(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		git        Git
		wantBase   string
		wantRef    string
		wantPrefix string
		wantErr    bool
	}{
		{
			name:     "explicit overrides win",
			env:      map[string]string{EnvSourceURL: "https://example.com/o/r", EnvSourceRef: "release"},
			git:      fakeGit("git@github.com:other/other.git", "feature"),
			wantBase: "https://example.com/o/r",
			wantRef:  "release",
		},
		{
			name: "github actions env",
			env: map[string]string{
				envGHServer: "https://github.com",
				envGHRepo:   "sigma/ideas",
				envGHRef:    "main",
			},
			git:      fakeGit("git@github.com:other/other.git", "feature"),
			wantBase: "https://github.com/sigma/ideas",
			wantRef:  "main",
		},
		{
			name:     "local git ssh remote normalized",
			env:      map[string]string{},
			git:      fakeGit("git@github.com:sigma/ideas.git", "main"),
			wantBase: "https://github.com/sigma/ideas",
			wantRef:  "main",
		},
		{
			name:     "local git https remote strips .git",
			env:      map[string]string{},
			git:      fakeGit("https://github.com/sigma/ideas.git", "topic"),
			wantBase: "https://github.com/sigma/ideas",
			wantRef:  "topic",
		},
		{
			name:     "per-field fallback: url override, ref from git",
			env:      map[string]string{EnvSourceURL: "https://example.com/o/r"},
			git:      fakeGit("git@github.com:other/other.git", "develop"),
			wantBase: "https://example.com/o/r",
			wantRef:  "develop",
		},
		{
			name:     "ref defaults to main when unresolved",
			env:      map[string]string{},
			git:      fakeGit("https://github.com/sigma/ideas", ""),
			wantBase: "https://github.com/sigma/ideas",
			wantRef:  "main",
		},
		{
			// A detached HEAD makes `rev-parse --abbrev-ref HEAD` report "HEAD"; that
			// must not become a dangling /edit/HEAD/ link — fall back to the branch.
			name:     "detached HEAD falls back to main",
			env:      map[string]string{},
			git:      fakeGit("https://github.com/sigma/ideas", "HEAD"),
			wantBase: "https://github.com/sigma/ideas",
			wantRef:  "main",
		},
		{
			// The real gitIn returns untrimmed stdout; Resolve must trim it.
			name:       "untrimmed git output is trimmed",
			env:        map[string]string{},
			git:        func(args ...string) (string, error) { return "  topic\n", nil },
			wantBase:   "topic", // remote get-url also returns "  topic\n" here; normalized
			wantRef:    "topic",
			wantPrefix: "topic", // --show-prefix answers "  topic\n" too
		},
		{
			name:     "trailing slash trimmed from base",
			env:      map[string]string{EnvSourceURL: "https://github.com/sigma/ideas/", EnvSourceRef: "main"},
			wantBase: "https://github.com/sigma/ideas",
			wantRef:  "main",
		},
		{
			// The bug this coordinate exists for: a bundle at docs/ inside its repo.
			name:       "prefix from local git show-prefix",
			env:        map[string]string{},
			git:        fakeGitIn("https://github.com/acme/iac", "main", "docs"),
			wantBase:   "https://github.com/acme/iac",
			wantRef:    "main",
			wantPrefix: "docs",
		},
		{
			name:       "prefix override wins over git",
			env:        map[string]string{EnvSourcePrefix: "content"},
			git:        fakeGitIn("https://github.com/acme/iac", "main", "docs"),
			wantBase:   "https://github.com/acme/iac",
			wantRef:    "main",
			wantPrefix: "content",
		},
		{
			// A checkout-less run: no git tier to answer, so the override is the only
			// way to a correct link — and it must work with no git at all.
			name: "prefix override with no git",
			env: map[string]string{
				EnvSourceURL:    "https://github.com/acme/iac",
				EnvSourceRef:    "main",
				EnvSourcePrefix: "docs",
			},
			git:        nil,
			wantBase:   "https://github.com/acme/iac",
			wantRef:    "main",
			wantPrefix: "docs",
		},
		{
			// A repo-root bundle: git reports an empty prefix, which must stay empty
			// rather than becoming "." — the URL join depends on it.
			name:       "repo-root bundle resolves an empty prefix",
			env:        map[string]string{},
			git:        fakeGitIn("https://github.com/sigma/ideas", "main", ""),
			wantBase:   "https://github.com/sigma/ideas",
			wantRef:    "main",
			wantPrefix: "",
		},
		{
			// An unresolved prefix must not fail the run the way an unresolved base
			// URL does: empty is the correct answer for the common repo-root case.
			name:       "no prefix anywhere is not an error",
			env:        map[string]string{EnvSourceURL: "https://github.com/sigma/ideas"},
			git:        nil,
			wantBase:   "https://github.com/sigma/ideas",
			wantRef:    "main",
			wantPrefix: "",
		},
		{
			name:    "fail loud when no base resolves",
			env:     map[string]string{},
			git:     fakeGit("", "main"),
			wantErr: true,
		},
		{
			name:    "fail loud with nil git and no env",
			env:     map[string]string{},
			git:     nil,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(env(tt.env), tt.git)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Resolve() = %+v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got.BaseURL != tt.wantBase {
				t.Errorf("BaseURL = %q, want %q", got.BaseURL, tt.wantBase)
			}
			if got.Ref != tt.wantRef {
				t.Errorf("Ref = %q, want %q", got.Ref, tt.wantRef)
			}
			if got.Prefix != tt.wantPrefix {
				t.Errorf("Prefix = %q, want %q", got.Prefix, tt.wantPrefix)
			}
		})
	}
}

// TestResolvePrefixNormalization: an operator writes the override by hand, so the
// separator-decorated forms they might reasonably type must all land on the one
// clean relative path the URL join expects.
func TestResolvePrefixNormalization(t *testing.T) {
	for _, tt := range []struct{ raw, want string }{
		{"docs", "docs"},
		{"/docs", "docs"},
		{"docs/", "docs"},
		{"/docs/", "docs"},
		{"./docs", "docs"},
		{"  docs  ", "docs"},
		{"sub/docs", "sub/docs"},
		{"sub//docs", "sub/docs"},
		{`sub\docs`, "sub/docs"}, // a Windows-style override still yields a URL path
		{"/", ""},
		{".", ""},
		// A prefix cannot escape the repo root: there is no correct URL for it, so it
		// degrades to the unprefixed link rather than minting "/edit/main/../x".
		{"..", ""},
		{"../outside", ""},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := Resolve(env(map[string]string{
				EnvSourceURL:    "https://h/r",
				EnvSourcePrefix: tt.raw,
			}), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.Prefix != tt.want {
				t.Errorf("Prefix for %q = %q, want %q", tt.raw, got.Prefix, tt.want)
			}
		})
	}
}

func TestResolveSSHURLScheme(t *testing.T) {
	got, err := Resolve(env(map[string]string{}), fakeGit("ssh://git@github.com/sigma/ideas.git", "main"))
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != "https://github.com/sigma/ideas" {
		t.Errorf("BaseURL = %q, want https://github.com/sigma/ideas", got.BaseURL)
	}
}
