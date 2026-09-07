package gdocs

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
