package climain

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func testApp(cmds map[string]Runner) App {
	return App{
		Prog:     "okftest",
		Version:  "1.2.3",
		Usage:    func(w io.Writer) { io.WriteString(w, "USAGE\n") },
		Commands: cmds,
	}
}

// run drives the whole dispatch and reports what the process would have done.
func run(a App, args ...string) (code int, out, errOut string) {
	var o, e bytes.Buffer
	code = a.Run(&o, &e, args)
	return code, o.String(), e.String()
}

// TestNoCommandIsAUsageError: a bare invocation is a mistake, not a request, so
// usage goes to stderr and the process fails.
func TestNoCommandIsAUsageError(t *testing.T) {
	code, out, errOut := run(testApp(nil))
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	if !strings.Contains(errOut, "USAGE") {
		t.Errorf("stderr = %q, want usage", errOut)
	}
}

// TestUnknownCommandNamesItself: the diagnostic has to say which command, or the
// user is left guessing at a typo.
func TestUnknownCommandNamesItself(t *testing.T) {
	code, _, errOut := run(testApp(map[string]Runner{"lint": nil}), "lnit")
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errOut, `unknown command "lnit"`) {
		t.Errorf("stderr = %q, should name the unknown command", errOut)
	}
	if !strings.Contains(errOut, "USAGE") {
		t.Errorf("stderr = %q, want usage after the diagnostic", errOut)
	}
}

// TestHelpIsARequest: -h is something the user asked for, so it answers on
// stdout and succeeds — a pipeline doing `okftool --help | less` must not fail.
func TestHelpIsARequest(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		code, out, errOut := run(testApp(nil), arg)
		if code != 0 {
			t.Errorf("%s: code = %d, want 0", arg, code)
		}
		if !strings.Contains(out, "USAGE") {
			t.Errorf("%s: stdout = %q, want usage", arg, out)
		}
		if errOut != "" {
			t.Errorf("%s: stderr = %q, want empty", arg, errOut)
		}
	}
}

func TestVersionReportsProgAndVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		code, out, _ := run(testApp(nil), arg)
		if code != 0 {
			t.Errorf("%s: code = %d, want 0", arg, code)
		}
		if strings.TrimSpace(out) != "okftest 1.2.3" {
			t.Errorf("%s: stdout = %q, want \"okftest 1.2.3\"", arg, out)
		}
	}
}

// TestCommandReceivesRemainingArgs: the subcommand name is consumed, everything
// after it is the command's own flag surface.
func TestCommandReceivesRemainingArgs(t *testing.T) {
	var got []string
	app := testApp(map[string]Runner{
		"lint": func(w io.Writer, args []string) (int, error) { got = args; return 0, nil },
	})
	if code, _, _ := run(app, "lint", "--fix", "a.md"); code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if strings.Join(got, " ") != "--fix a.md" {
		t.Errorf("args = %v, want [--fix a.md]", got)
	}
}

// TestCommandExitCodePassesThrough: a clean non-zero code (lint found findings)
// is not an error, and must not be reported as one.
func TestCommandExitCodePassesThrough(t *testing.T) {
	app := testApp(map[string]Runner{
		"lint": func(w io.Writer, args []string) (int, error) { return 1, nil },
	})
	code, _, errOut := run(app, "lint")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if errOut != "" {
		t.Errorf("stderr = %q; a findings exit code is not an error", errOut)
	}
}

// TestErrorIsPrefixedAndFails pins the two halves of error reporting: the message
// is attributed to the program, and a command that reports an error but forgets
// to set a code still fails the process.
func TestErrorIsPrefixedAndFails(t *testing.T) {
	app := testApp(map[string]Runner{
		"lint": func(w io.Writer, args []string) (int, error) { return 0, errors.New("bundle not found") },
	})
	code, _, errOut := run(app, "lint")
	if code != 1 {
		t.Errorf("code = %d, want 1 — an error must not exit 0", code)
	}
	if strings.TrimSpace(errOut) != "okftest: bundle not found" {
		t.Errorf("stderr = %q, want \"okftest: bundle not found\"", errOut)
	}
}

// TestErrorKeepsAnExplicitCode: a command that sets its own failing code keeps
// it — flag parsing reports 2, and that must survive the error path.
func TestErrorKeepsAnExplicitCode(t *testing.T) {
	app := testApp(map[string]Runner{
		"lint": func(w io.Writer, args []string) (int, error) { return 2, errors.New("bad flag") },
	})
	if code, _, _ := run(app, "lint"); code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}
