package command

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The command entrypoints now take an io.Writer, so a test can drive the real
// (w, args) surface — flag parsing, rendering, and the exit code — off a buffer,
// instead of the algorithm helpers underneath. These tests cross the same seam
// main.go does.

// TestFmtCheckReportsAndExits: `fmt --check` on a bundle that needs formatting
// exits 1 and names the files on stdout; --format json emits the would_reformat
// envelope. The okf105 fixture has citation numbering fmt would renumber.
func TestFmtCheckReportsAndExits(t *testing.T) {
	dir := fixtureDir("okf105")

	var buf bytes.Buffer
	code, err := Fmt(&buf, []string{"--bundle", dir})
	if err != nil {
		t.Fatalf("Fmt: %v", err)
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1 when files need formatting", code)
	}
	if !strings.Contains(buf.String(), "would reformat") {
		t.Errorf("stdout = %q, want it to name files that would reformat", buf.String())
	}

	var jbuf bytes.Buffer
	code, err = Fmt(&jbuf, []string{"--bundle", dir, "--format", "json"})
	if err != nil {
		t.Fatalf("Fmt --format json: %v", err)
	}
	if code != 1 {
		t.Errorf("json exit code = %d, want 1", code)
	}
	var env struct {
		WouldReformat []string `json:"would_reformat"`
	}
	if err := json.Unmarshal(jbuf.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, jbuf.String())
	}
	if len(env.WouldReformat) == 0 {
		t.Errorf("would_reformat = %v, want at least one file", env.WouldReformat)
	}
}

// TestIndexCheckReportsAndExits: `index --check` on a bundle with a stale index
// exits 1 and reports OKF106; a clean bundle exits 0. Exercises the shared
// findingsExit(threshold Info) path through the entrypoint.
func TestIndexCheckReportsAndExits(t *testing.T) {
	var buf bytes.Buffer
	code, err := Index(&buf, []string{"--bundle", fixtureDir("okf106")})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1 for a stale index", code)
	}
	if !strings.Contains(buf.String(), "OKF106") {
		t.Errorf("stdout = %q, want it to report OKF106", buf.String())
	}

	var clean bytes.Buffer
	code, err = Index(&clean, []string{"--bundle", fixtureDir("happy")})
	if err != nil {
		t.Fatalf("Index (clean): %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0 for a clean bundle\n%s", code, clean.String())
	}
}
