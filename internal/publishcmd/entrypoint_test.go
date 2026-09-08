package publishcmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cross the same seam main.go does — the real (out, args) surface,
// with its flag parsing, its policy and its exit code. None of it was reachable
// while the body sat in package main: the only thing cmd/okfpub could test was the
// progress reporter, which is also the only piece that had been given a writer.

// bundleDir materializes a minimal publishable bundle and returns its root.
func bundleDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The banner is on by default and resolves against the environment or local git,
	// neither of which a temp dir has; these tests are about the entrypoint, not
	// about source resolution, which has its own tests.
	write("okf.toml", "[banner]\nenabled = false\n")
	write("index.md", "---\nokf_version: \"0.1\"\n---\n# Root\n\nBody.\n")
	return dir
}

// TestRunPublishesAndReportsSuccess is the happy path through the entrypoint: a
// fake-backend publish exits 0 and prints the summary the operator reads.
func TestRunPublishesAndReportsSuccess(t *testing.T) {
	var buf bytes.Buffer
	code, err := Run(&buf, []string{"--backend", "fake", "--bundle", bundleDir(t)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0 for a clean publish", code)
	}
	if got := buf.String(); !strings.Contains(got, "published") || !strings.Contains(got, "via fake backend") {
		t.Errorf("stdout = %q, want the publish summary naming the backend", got)
	}
}

// TestSelectIsRejectedOffGDocs pins a policy that lived only in main: --select is
// a fan-out flag, and only the document backend fans out. Accepting it elsewhere
// would silently publish the whole bundle while the operator believed they had
// narrowed it.
func TestSelectIsRejectedOffGDocs(t *testing.T) {
	var buf bytes.Buffer
	code, err := Run(&buf, []string{"--backend", "fake", "--bundle", bundleDir(t), "--select", "docs"})
	if err == nil {
		t.Fatal("--select on a non-gdocs backend must be an error, not a silent full publish")
	}
	if code != 2 {
		t.Errorf("exit code = %d, want 2 for a misused flag", code)
	}
	if !strings.Contains(err.Error(), "--select") {
		t.Errorf("error = %q, want it to name the offending flag", err)
	}
}

// TestDryRunRedirectsToTheFilesystem pins the other policy that lived only in
// main: --dry-run means "publish nothing", and for every backend but gdocs that is
// expressed by REWRITING the backend kind to the filesystem export. The summary
// must therefore name fs, not the backend the operator asked for.
func TestDryRunRedirectsToTheFilesystem(t *testing.T) {
	out := filepath.Join(t.TempDir(), "export")
	var buf bytes.Buffer
	code, err := Run(&buf, []string{
		"--backend", "notion", "--bundle", bundleDir(t), "--dry-run", "--out", out,
	})
	if err != nil {
		t.Fatalf("Run --dry-run: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if got := buf.String(); !strings.Contains(got, "via fs backend") {
		t.Errorf("stdout = %q, want the run to report the fs backend it was redirected to", got)
	}
	// --dry-run must not have reached Notion; it needs no credentials at all, which
	// is the property that makes it safe to run anywhere.
	if _, err := os.Stat(out); err != nil {
		t.Errorf("--dry-run should have exported to %s: %v", out, err)
	}
}

// TestUnknownFlagExitsTwo proves flag parsing is now inside the test surface: a
// typo'd flag reports a usage error and exits 2 rather than 1.
func TestUnknownFlagExitsTwo(t *testing.T) {
	var buf bytes.Buffer
	code, err := Run(&buf, []string{"--no-such-flag"})
	if err == nil {
		t.Fatal("an unknown flag must be an error")
	}
	if code != 2 {
		t.Errorf("exit code = %d, want 2 for a usage error", code)
	}
}

// TestHelpIsNotAFailure: -h is a request, and flag has already written the usage
// to the caller's writer, so it exits 0 with nothing to add.
func TestHelpIsNotAFailure(t *testing.T) {
	var buf bytes.Buffer
	code, err := Run(&buf, []string{"-h"})
	if err != nil {
		t.Errorf("-h should not be an error, got %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0 for -h", code)
	}
	if !strings.Contains(buf.String(), "-backend") {
		t.Errorf("help went somewhere other than the caller's writer: %q", buf.String())
	}
}

// TestUsageGoesToTheCallersWriter keeps main.go honest: it routes usage to stderr
// or stdout depending on why it is printing, which only works if Usage takes a
// writer at all.
func TestUsageGoesToTheCallersWriter(t *testing.T) {
	var buf bytes.Buffer
	Usage(&buf)
	for _, want := range []string{"okfpub", "--backend", "NOTION_TOKEN"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("usage text is missing %q", want)
		}
	}
}

// TestAreasFlagSteersTheRegistryThePublishUses is the point of routing --areas
// through bundle.Load. The flag used to feed a SECOND parse into pipeline.Config,
// whose only consumer was the diagnostic line below — so pointing it elsewhere
// changed what the run reported while the publish went on using <root>/areas.json.
// The report and the publish now read one registry.
func TestAreasFlagSteersTheRegistryThePublishUses(t *testing.T) {
	dir := bundleDir(t)
	if err := os.WriteFile(filepath.Join(dir, "CONTEXT.md"),
		[]byte("# Glossary\n\n**Root KEK**: the root key-encryption key.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The registry at the bundle root marks no glossary host.
	if err := os.WriteFile(filepath.Join(dir, "areas.json"),
		[]byte(`{"docs": {"directory": "docs", "type": "adr"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// The one --areas points at does.
	alt := filepath.Join(t.TempDir(), "other-areas.json")
	if err := os.WriteFile(alt,
		[]byte(`{"glossary": {"file": "CONTEXT.md", "type": "glossary", "role": "glossary"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if _, err := Run(&buf, []string{"--backend", "fake", "--bundle", dir, "--areas", alt}); err != nil {
		t.Fatalf("Run --areas: %v", err)
	}
	if !strings.Contains(buf.String(), "glossary/anchor host: CONTEXT.md") {
		t.Errorf("stdout = %q, want the host from the registry --areas named", buf.String())
	}

	// Without the flag the root registry applies, and it marks no host.
	var base bytes.Buffer
	if _, err := Run(&base, []string{"--backend", "fake", "--bundle", dir}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(base.String(), "glossary/anchor host") {
		t.Errorf("stdout = %q, want no host from a registry that marks none", base.String())
	}
}

// TestAreasFlagPointingNowhereIsAnError: naming a file that does not exist must
// fail rather than silently falling back to the bundle root's registry and
// publishing against a different contract than the operator asked for.
func TestAreasFlagPointingNowhereIsAnError(t *testing.T) {
	var buf bytes.Buffer
	_, err := Run(&buf, []string{
		"--backend", "fake", "--bundle", bundleDir(t),
		"--areas", filepath.Join(t.TempDir(), "absent.json"),
	})
	if err == nil {
		t.Fatal("--areas naming a missing file must be an error")
	}
	if !strings.Contains(err.Error(), "areas") {
		t.Errorf("error = %q, want it to name the registry it could not read", err)
	}
}
