package notion

import "testing"

// This file bridges the internal test package to the external one. Every fake in
// this package is declared in `package notion` — the fake server included — so
// nothing outside it can build one. The conformance kit's caller lives in
// `package notion_test`, because importing the kit from `package notion` would
// close an import cycle: the kit reaches the pipeline, and the pipeline knows
// every concrete backend, including this one.
//
// Identifiers declared in a package's internal test files are visible to its
// external test files, so one exported constructor is enough and the fake stays
// where it is.

// NewConformanceSubject builds a backend aimed at a fresh fake Notion server, and
// returns it with a probe reporting how many WRITES it has issued.
func NewConformanceSubject(t *testing.T) (*Backend, func() int) {
	t.Helper()
	f := newFakeNotion()
	be := newServer(t, f)
	return be, f.writeCount
}
