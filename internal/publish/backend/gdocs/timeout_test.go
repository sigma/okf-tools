package gdocs_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sigma/okf-tools/internal/publish/backend/gdocs"
)

// TestStalledRequestFailsRatherThanHanging is #184 for this backend. It was never
// observed hanging, but nothing about it prevented the same: one half-dead
// connection and a publish waits on a reply that never comes.
func TestStalledRequestFailsRatherThanHanging(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer srv.Close()

	be, err := gdocs.New(context.Background(), gdocs.Config{
		DriveID: testDriveID, Bundle: "testbundle", Selection: "concepts",
		DocsEndpoint: srv.URL, DriveEndpoint: srv.URL,
		HTTPClient: &http.Client{}, RequestTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- be.Provision(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("provisioning against a stalled server returned no error")
		}
		if !strings.Contains(err.Error(), "timeout") {
			t.Errorf("error = %v, want it to name the timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request hung: no deadline bounded it")
	}
}

// The default is applied when a caller names none, so a real run is bounded
// without opting in — the whole point of the fix, and the wiring nothing else
// would catch.
func TestDefaultRequestTimeoutIsApplied(t *testing.T) {
	be, err := gdocs.New(context.Background(), gdocs.Config{
		DriveID: testDriveID, Bundle: "b", Selection: "s", HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if got := be.RequestTimeout(); got != gdocs.DefaultRequestTimeout {
		t.Errorf("request timeout = %v, want the default %v", got, gdocs.DefaultRequestTimeout)
	}
}
