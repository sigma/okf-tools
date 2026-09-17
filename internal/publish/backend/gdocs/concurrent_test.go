package gdocs_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/sigma/okf-tools/internal/publish/backend"
	"github.com/sigma/okf-tools/internal/publish/backend/gdocs"
)

// The Google Docs backend must NOT take transactions concurrently: it places
// tabs by the order their groups land, so it relies on the one-at-a-time drain
// (#212). This guards against the role being satisfied by accident.
func TestGDocsDeclaresNoConcurrency(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	t.Cleanup(srv.Close)
	be, err := gdocs.New(context.Background(), gdocs.Config{
		DriveID: testDriveID, Bundle: "concurrency", Selection: "all",
		DocsEndpoint: srv.URL, DriveEndpoint: srv.URL, HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, ok := any(be).(backend.ConcurrentExecutor); ok {
		t.Fatal("gdocs declares a concurrency bound; its tab order depends on a sequential drain")
	}
}
