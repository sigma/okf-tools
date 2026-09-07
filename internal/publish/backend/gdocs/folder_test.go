package gdocs_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/publish/backend/gdocs"
	"github.com/sigma/okf-tools/internal/publish/pipeline"
)

// testFolderID is a folder INSIDE testDriveID: a legal write target that the
// Drive API rejects as a search corpus (#168).
const testFolderID = "1FOLDER"

func folderBackend(t *testing.T, srv, target string) *gdocs.Backend {
	t.Helper()
	be, err := gdocs.New(context.Background(), gdocs.Config{
		DriveID: target, Bundle: "testbundle", Selection: "concepts",
		DocsEndpoint: srv, DriveEndpoint: srv, HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return be
}

// TestPublishIntoAFolder is the whole point of #168: the configured destination
// is a folder inside a shared drive, which fails today because the folder id is
// passed as the search corpus.
func TestPublishIntoAFolder(t *testing.T) {
	fake := newFakeGoogle(t)
	fake.addFolder(testFolderID, testDriveID)
	srv := fake.server()
	defer srv.Close()

	be := folderBackend(t, srv.URL, testFolderID)
	b := loadBundle(t, testBundle())
	if _, err := pipeline.Run(context.Background(), be, b); err != nil {
		t.Fatalf("run: %v", err)
	}

	docID := be.DocumentID()
	if parents := fake.parentsOf(docID); len(parents) != 1 || parents[0] != testFolderID {
		t.Errorf("the document landed in %v, want [%s]", parents, testFolderID)
	}
	for _, id := range fake.fileIDs() {
		if parents := fake.parentsOf(id); len(parents) != 1 || parents[0] != testFolderID {
			t.Errorf("file %s landed in %v, want [%s]", id, parents, testFolderID)
		}
	}
	// Regression on the actual bug: the corpus is a DRIVE id, never the parent.
	for _, corpus := range fake.corpora {
		if corpus != testDriveID {
			t.Errorf("files.list searched corpus %q, want the drive id %q", corpus, testDriveID)
		}
	}
	if len(fake.corpora) == 0 {
		t.Fatal("no lookup happened, so nothing was asserted")
	}
}

// TestFolderPublishIsIdempotent: a second run finds the document it created in
// the folder rather than creating a second one — the lookup must match on the
// resolved parent, not on the drive root.
func TestFolderPublishIsIdempotent(t *testing.T) {
	fake := newFakeGoogle(t)
	fake.addFolder(testFolderID, testDriveID)
	srv := fake.server()
	defer srv.Close()

	b := loadBundle(t, testBundle())
	first := folderBackend(t, srv.URL, testFolderID)
	if _, err := pipeline.Run(context.Background(), first, b); err != nil {
		t.Fatalf("first run: %v", err)
	}
	created := len(fake.fileIDs())

	second := folderBackend(t, srv.URL, testFolderID)
	if _, err := pipeline.Run(context.Background(), second, b); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := len(fake.fileIDs()); got != created {
		t.Errorf("the second run created %d extra Drive file(s)", got-created)
	}
	if second.DocumentID() != first.DocumentID() {
		t.Errorf("the second run found %s, want the first run's %s",
			second.DocumentID(), first.DocumentID())
	}
}

// TestPublishIntoADriveRoot pins the claim that the two cases collapse into one
// code path: a drive root is its own parent AND its own corpus.
func TestPublishIntoADriveRoot(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	be := folderBackend(t, srv.URL, testDriveID)
	b := loadBundle(t, testBundle())
	if _, err := pipeline.Run(context.Background(), be, b); err != nil {
		t.Fatalf("run: %v", err)
	}
	if parents := fake.parentsOf(be.DocumentID()); len(parents) != 1 || parents[0] != testDriveID {
		t.Errorf("the document landed in %v, want [%s]", parents, testDriveID)
	}
	for _, corpus := range fake.corpora {
		if corpus != testDriveID {
			t.Errorf("files.list searched corpus %q, want %q", corpus, testDriveID)
		}
	}
}

// TestDriveRootWithoutADriveIDField covers the open question #168 could not
// verify: if files.get on a drive root omits driveId, the resolver must fall
// back to drives.get rather than publishing with an empty corpus.
func TestDriveRootWithoutADriveIDField(t *testing.T) {
	fake := newFakeGoogle(t)
	fake.containers[testDriveID].hideDriveID = true
	srv := fake.server()
	defer srv.Close()

	be := folderBackend(t, srv.URL, testDriveID)
	b := loadBundle(t, testBundle())
	if _, err := pipeline.Run(context.Background(), be, b); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, corpus := range fake.corpora {
		if corpus != testDriveID {
			t.Errorf("files.list searched corpus %q, want %q", corpus, testDriveID)
		}
	}
}

// TestMyDriveFolderIsRejected: a folder that is on no shared drive reports no
// driveId AND is no drive, which is the My Drive case (#149) — the one that used
// to fail much later with a misleading 403 storageQuotaExceeded.
func TestMyDriveFolderIsRejected(t *testing.T) {
	fake := newFakeGoogle(t)
	orphan := fake.addFolder("1MYDRIVE", "")
	orphan.hideDriveID = true
	srv := fake.server()
	defer srv.Close()

	be := folderBackend(t, srv.URL, orphan.id)
	_, err := pipeline.Run(context.Background(), be, loadBundle(t, testBundle()))
	if err == nil {
		t.Fatal("publishing into a folder on no shared drive succeeded")
	}
	if !strings.Contains(err.Error(), "no shared drive") {
		t.Errorf("the error does not name the cause: %v", err)
	}
	if len(fake.files) != 0 {
		t.Errorf("a rejected destination still saw %d file(s) created", len(fake.files))
	}
}

// TestNonFolderDestinationIsRejected: pointing GDRIVE_FOLDER_ID at a document
// fails at resolution, naming the cause, rather than deep inside provisioning.
func TestNonFolderDestinationIsRejected(t *testing.T) {
	fake := newFakeGoogle(t)
	docID := fake.addDocumentFile(testDriveID)
	srv := fake.server()
	defer srv.Close()

	be := folderBackend(t, srv.URL, docID)
	_, err := pipeline.Run(context.Background(), be, loadBundle(t, testBundle()))
	if err == nil {
		t.Fatal("publishing into a document id succeeded")
	}
	if !strings.Contains(err.Error(), "folder") {
		t.Errorf("the error does not name the cause: %v", err)
	}
}

// TestDryRunNamesTheFolderAndTheDrive: once the two can differ, the dump has to
// say which is which — and a drive root, where they do not differ, is named as a
// drive rather than as a folder that happens to share its id.
func TestDryRunNamesTheFolderAndTheDrive(t *testing.T) {
	fake := newFakeGoogle(t)
	fake.addFolder(testFolderID, testDriveID)
	srv := fake.server()
	defer srv.Close()

	var dump strings.Builder
	be, err := gdocs.New(context.Background(), gdocs.Config{
		DriveID: testFolderID, Bundle: "testbundle", Selection: "concepts",
		DocsEndpoint: srv.URL, DriveEndpoint: srv.URL, HTTPClient: &http.Client{},
		DryRunWriter: &dump,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := pipeline.Run(context.Background(), be, loadBundle(t, testBundle())); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(fake.files) != 0 {
		t.Errorf("a dry run created %d Drive file(s)", len(fake.files))
	}
	out := dump.String()
	if want := fmt.Sprintf("folder %s (drive %s)", testFolderID, testDriveID); !strings.Contains(out, want) {
		t.Errorf("the dump does not mention %q:\n%s", want, firstLines(out))
	}
}

// TestDryRunNamesADriveRootAsADrive pins the other half: no "folder X (drive X)".
func TestDryRunNamesADriveRootAsADrive(t *testing.T) {
	fake := newFakeGoogle(t)
	srv := fake.server()
	defer srv.Close()

	var dump strings.Builder
	be, err := gdocs.New(context.Background(), gdocs.Config{
		DriveID: testDriveID, Bundle: "testbundle", Selection: "concepts",
		DocsEndpoint: srv.URL, DriveEndpoint: srv.URL, HTTPClient: &http.Client{},
		DryRunWriter: &dump,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := pipeline.Run(context.Background(), be, loadBundle(t, testBundle())); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := firstLines(dump.String())
	if !strings.Contains(out, "in drive "+testDriveID) || strings.Contains(out, "folder") {
		t.Errorf("a drive root should be named as a drive: %q", out)
	}
}

func firstLines(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}
