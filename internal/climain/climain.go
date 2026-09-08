// Package climain holds the argv-to-exit-code shell both binaries wear.
//
// okftool and okfpub had each moved their command bodies behind a testable seam
// — internal/command and internal/publishcmd exist for exactly that reason — and
// then left the dispatch itself in package main, where no test can reach it.
// Twice. The two loops differed only in the program name and the command table,
// yet between them they decide every convention a user actually meets first: that
// a missing command is exit 2 and prints usage to stderr, that -h is a request
// rather than a failure and prints to stdout, that an error is reported as
// "prog: message", and that a command returning an error but a zero code still
// fails the process.
//
// Behind one seam, those become assertions instead of conventions maintained by
// hand in two files.
package climain

import (
	"fmt"
	"io"
)

// Runner is one subcommand: it writes its output and reports a process exit
// code. The error is rendered by Run; a Runner returning one need not print it.
type Runner func(out io.Writer, args []string) (int, error)

// App is a binary's command-line surface: what it is called, what it reports for
// --version, how it describes itself, and what it can do.
type App struct {
	Prog     string
	Version  string
	Usage    func(io.Writer)
	Commands map[string]Runner
}

// Run resolves args[0] against the command table and runs it, reporting the
// process exit code. args is os.Args[1:]; out and errOut are the process streams,
// taken as parameters so a test drives the whole dispatch rather than only the
// command bodies behind it.
//
// No command at all, and an unknown one, are both usage errors (exit 2) reported
// on errOut. help and version are requests: they answer on out and exit 0.
func (a App) Run(out, errOut io.Writer, args []string) int {
	if len(args) == 0 {
		a.Usage(errOut)
		return 2
	}

	name, rest := args[0], args[1:]
	switch name {
	case "version", "--version", "-v":
		fmt.Fprintln(out, a.Prog+" "+a.Version)
		return 0
	case "help", "-h", "--help":
		a.Usage(out)
		return 0
	}

	run, ok := a.Commands[name]
	if !ok {
		fmt.Fprintf(errOut, "%s: unknown command %q\n\n", a.Prog, name)
		a.Usage(errOut)
		return 2
	}

	code, err := run(out, rest)
	if err != nil {
		fmt.Fprintln(errOut, a.Prog+": "+err.Error())
		// A command that reports a failure must not exit 0 because it forgot to set
		// a code — the error is the authority on whether the run failed.
		if code == 0 {
			code = 1
		}
	}
	return code
}
