package notion_test

import (
	"testing"

	"github.com/sigma/okf-tools/internal/publish/backend/backendtest"
	"github.com/sigma/okf-tools/internal/publish/backend/notion"
)

// TestConformance runs the cross-backend kit against this backend, through the
// fake Notion server the rest of the suite uses. It lives in the EXTERNAL test
// package because the kit reaches the pipeline, which knows every concrete
// backend including this one; see export_test.go for the bridge.
func TestConformance(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backendtest.Subject {
		be, writes := notion.NewConformanceSubject(t)
		return backendtest.Subject{Backend: be, Writes: writes}
	})
}
