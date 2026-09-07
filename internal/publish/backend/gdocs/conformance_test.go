package gdocs_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/sigma/okf-tools/internal/publish/backend/backendtest"
	"github.com/sigma/okf-tools/internal/publish/backend/gdocs"
)

// TestConformance runs the cross-backend kit against this backend, using the
// same fake Drive/Docs surface the rest of this suite uses. The kit owns the
// fixtures and the properties; this file owns only "how do you build one".
func TestConformance(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backendtest.Subject {
		fake := newFakeGoogle(t)
		srv := fake.server()
		t.Cleanup(srv.Close)

		be, err := gdocs.New(context.Background(), gdocs.Config{
			DriveID: testDriveID, Bundle: "conformance", Selection: "all",
			DocsEndpoint: srv.URL, DriveEndpoint: srv.URL, HTTPClient: &http.Client{},
		})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		return backendtest.Subject{
			Backend: be,
			// Every write this backend makes is a batchUpdate, so the fake's count is
			// the write count.
			Writes: func() int { return fake.batchUpdateCount() },
		}
	})
}
