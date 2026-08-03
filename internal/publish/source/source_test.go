package source

import "testing"

// fakeGit builds a Git that answers `remote get-url origin` and
// `rev-parse --abbrev-ref HEAD` from the given values; an empty value makes that
// subcommand fail (as if unavailable), so tests can exercise the fall-through.
func fakeGit(remote, branch string) Git {
	return func(args ...string) (string, error) {
		switch {
		case len(args) >= 2 && args[0] == "remote" && args[1] == "get-url":
			if remote == "" {
				return "", errNoGit
			}
			return remote + "\n", nil
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
		name     string
		env      map[string]string
		git      Git
		wantBase string
		wantRef  string
		wantErr  bool
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
			name:     "untrimmed git output is trimmed",
			env:      map[string]string{},
			git:      func(args ...string) (string, error) { return "  topic\n", nil },
			wantBase: "topic", // remote get-url also returns "  topic\n" here; normalized
			wantRef:  "topic",
		},
		{
			name:     "trailing slash trimmed from base",
			env:      map[string]string{EnvSourceURL: "https://github.com/sigma/ideas/", EnvSourceRef: "main"},
			wantBase: "https://github.com/sigma/ideas",
			wantRef:  "main",
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
