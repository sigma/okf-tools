package fake_test

import (
	"testing"

	"github.com/sigma/okf-tools/internal/publish/backend/backendtest"
	"github.com/sigma/okf-tools/internal/publish/backend/fake"
)

// TestConformance runs the cross-backend kit against the in-memory fake.
//
// It carries FEWER properties than the real backends: its scan is canned, so
// both republish properties are skipped, and what remains is the publish, the
// anchors and the expected nodes. That is worth knowing when reading this fake
// as a stand-in elsewhere in the suite — it conforms on what it can express,
// which is not everything the real backends must.
func TestConformance(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backendtest.Subject {
		be := fake.New()
		return backendtest.Subject{
			Backend: be,
			// No Writes probe, deliberately: this backend's Scan returns the canned
			// state it was seeded with and never reports what it wrote, so every run
			// rewrites everything by design. Near-noop rests on the scan finding the
			// prior state, so the property is not applicable here — and it is the one
			// property the real backends must carry that their stand-in cannot.
		}
	})
}
