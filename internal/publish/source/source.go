// Package source resolves the source-repo web coordinates a mirrored page's
// generated-page disclaimer banner deep-links into (sigma/ideas ADR-0015).
//
// It lives deliberately outside the pure Generation core (internal/publish/graph),
// which reads no environment so it stays deterministic and portable to a reusable
// external Action. Resolution reads the environment and, as a last resort, local
// git; the resolved value is then threaded into the planner as plain data. This is
// the "bins' job" the ADR names: the same discipline that confines NOTION_* to the
// executor config keeps GITHUB_* / git out of the planner.
package source

import (
	"fmt"
	"path"
	"strings"
)

// Source is the resolved source-repo web coordinates a banner links against: the
// web base URL of the repo (e.g. https://github.com/sigma/ideas), the branch ref
// the GitHub /edit/ link targets, and the bundle root's own path within the repo
// work tree.
//
// That third coordinate is what makes the link correct for a bundle that is not
// the repo root: a node knows only its bundle-relative path, so without a prefix
// the two only coincide when the bundle *is* the repo root, and every banner on a
// docs/-rooted bundle deep-links to a path that does not exist.
type Source struct {
	BaseURL string
	Ref     string
	// Prefix is the bundle root relative to the repo work tree root, as a clean
	// forward-slash path with no leading or trailing separator ("docs",
	// "sub/docs"). Empty means the bundle root is the repo root.
	Prefix string
}

// Git runs a git subcommand and returns its stdout (untrimmed) or an error. It is
// injected so resolution stays unit-testable without a real checkout.
type Git func(args ...string) (string, error)

// Environment variables the resolver reads. The explicit overrides win over the
// GitHub Actions environment, which wins over local git — resolved per field, so a
// partial override still falls back for the other field.
const (
	// EnvSourceURL overrides the repo web base URL (tier 1).
	EnvSourceURL = "OKF_SOURCE_URL"
	// EnvSourceRef overrides the branch ref (tier 1).
	EnvSourceRef = "OKF_SOURCE_REF"
	// EnvSourcePrefix overrides the bundle root's path within the repo (tier 1).
	// It is the only way to get a correct deep-link out of a checkout-less run,
	// where the local-git tier cannot answer.
	EnvSourcePrefix = "OKF_SOURCE_PREFIX"

	envGHServer = "GITHUB_SERVER_URL"
	envGHRepo   = "GITHUB_REPOSITORY"
	envGHRef    = "GITHUB_REF_NAME"
)

// defaultRef is the branch the /edit/ link targets when none resolves. ADR-0015
// fixes the banner's link to the syncing branch (main), never a commit SHA, since
// /edit/ requires a branch and "edit this frozen snapshot" is the wrong target.
const defaultRef = "main"

// Resolve determines the source base URL and ref by the ADR-0015 precedence,
// resolved independently per field:
//
//  1. explicit override  — OKF_SOURCE_URL / OKF_SOURCE_REF / OKF_SOURCE_PREFIX
//  2. GitHub Actions env — {GITHUB_SERVER_URL}/{GITHUB_REPOSITORY}, GITHUB_REF_NAME
//  3. local git          — `git remote get-url origin` (ssh→https), current branch,
//     and `git rev-parse --show-prefix` for the bundle root's path within the repo
//
// The prefix has no GitHub Actions tier because no GITHUB_* variable carries it:
// the value is a property of where the bundle sits, not of the run. Under Actions
// the git tier answers it anyway, since actions/checkout provides a work tree.
//
// It fails loud when no base URL resolves by any tier: a mirror whose pages must
// deep-link to their source cannot publish a banner with a dangling link, so this
// surfaces the misconfiguration rather than emitting a broken one. A ref that
// resolves nowhere falls back to the syncing branch rather than failing, keeping
// the link usable. An unresolved prefix stays empty rather than failing: that is
// the correct answer for a repo-root bundle, which is the common case, so failing
// would break the majority to protect the minority.
//
// git is expected to run in the bundle root — `rev-parse --show-prefix` reports
// the working directory's path relative to the repo toplevel, so the bundle's own
// location is read directly rather than inferred by diffing two absolute paths.
func Resolve(getenv func(string) string, git Git) (Source, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	var s Source

	switch {
	case getenv(EnvSourceURL) != "":
		s.BaseURL = getenv(EnvSourceURL)
	case getenv(envGHServer) != "" && getenv(envGHRepo) != "":
		s.BaseURL = getenv(envGHServer) + "/" + getenv(envGHRepo)
	case git != nil:
		if out, err := git("remote", "get-url", "origin"); err == nil {
			s.BaseURL = normalizeRemote(out)
		}
	}

	switch {
	case getenv(EnvSourceRef) != "":
		s.Ref = getenv(EnvSourceRef)
	case getenv(envGHRef) != "":
		s.Ref = getenv(envGHRef)
	case git != nil:
		if out, err := git("rev-parse", "--abbrev-ref", "HEAD"); err == nil {
			s.Ref = strings.TrimSpace(out)
		}
	}

	switch {
	case getenv(EnvSourcePrefix) != "":
		s.Prefix = normalizePrefix(getenv(EnvSourcePrefix))
	case git != nil:
		if out, err := git("rev-parse", "--show-prefix"); err == nil {
			s.Prefix = normalizePrefix(out)
		}
	}

	s.BaseURL = strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	if s.BaseURL == "" {
		return Source{}, fmt.Errorf(
			"source: no repo base URL resolved — set %s, run under GitHub Actions, or publish from a git checkout with an 'origin' remote",
			EnvSourceURL)
	}
	// A detached HEAD makes `rev-parse --abbrev-ref HEAD` report the literal
	// "HEAD", which would mint a dangling /edit/HEAD/<path> link. Fall back to the
	// syncing branch, matching the empty-ref case — /edit/ needs a real branch.
	if s.Ref == "" || s.Ref == "HEAD" {
		s.Ref = defaultRef
	}
	return s, nil
}

// normalizePrefix reduces a raw prefix — an operator's OKF_SOURCE_PREFIX, or
// git's trailing-slashed `--show-prefix` output — to the clean forward-slash
// relative path the URL join expects: no leading or trailing separator, no "./".
// Backslashes are folded to forward slashes so a Windows-style override still
// yields a valid URL path.
//
// A value that escapes the repo root ("..", "../x") normalizes to empty: it
// cannot name a path inside the repo, so there is no URL it could correctly
// produce, and an unprefixed link is the same fallback an unresolved prefix gets.
func normalizePrefix(raw string) string {
	p := strings.TrimSpace(raw)
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	p = path.Clean(p)
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return ""
	}
	return p
}

// normalizeRemote turns a git remote URL into a web base URL: an scp-style ssh
// remote (git@github.com:owner/repo.git) or an ssh:// URL becomes
// https://host/owner/repo, an https remote keeps its scheme, and a trailing ".git"
// is dropped in either case.
func normalizeRemote(raw string) string {
	u := strings.TrimSpace(raw)
	u = strings.TrimSuffix(u, ".git")
	switch {
	case strings.HasPrefix(u, "git@"):
		u = strings.TrimPrefix(u, "git@")
		u = strings.Replace(u, ":", "/", 1) // host:owner/repo -> host/owner/repo
		return "https://" + u
	case strings.HasPrefix(u, "ssh://"):
		u = strings.TrimPrefix(u, "ssh://")
		u = strings.TrimPrefix(u, "git@")
		return "https://" + u
	default:
		return u
	}
}
