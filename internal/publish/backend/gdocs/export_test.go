package gdocs

import "time"

// Internals the external test package asserts against. Re-declaring these as
// literals over there would let a rename pass the assertion silently — the
// sentinel's whole job is to never reach a request, so the test has to watch the
// real value.
const (
	DeferredHeadingID = deferredHeadingID
	DryRunHeadingID   = dryRunHeadingID
	// MaxTabTitle and BodyBase are asserted against by the external tests. The
	// FAKE deliberately keeps its own copies — it models the server, and a fake
	// that borrows the client's constants cannot disagree with it — but a test
	// asserting on the backend's output must watch the backend's real values.
	MaxTabTitle = maxTabTitle
	BodyBase    = bodyBase
)

// RequestTimeout reports the per-attempt deadline the client was built with, so a
// test can assert New WIRED the default rather than merely that the default
// exists (#184).
func (b *Backend) RequestTimeout() time.Duration { return b.c.timeout }
