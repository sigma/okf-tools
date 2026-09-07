package gdocs

// Internals the external test package asserts against. Re-declaring these as
// literals over there would let a rename pass the assertion silently — the
// sentinel's whole job is to never reach a request, so the test has to watch the
// real value.
const (
	DeferredHeadingID = deferredHeadingID
	DryRunHeadingID   = dryRunHeadingID
)
