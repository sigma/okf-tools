package notion

import (
	"context"
	"testing"

	"github.com/sigma/okf-tools/internal/schema"
)

// TestProvisionAddsSelfDescribingColumns: okfpub's own self-describing columns
// (path, hash, hashes, anchors) are provisioned regardless of what the consumer's
// schema declares — they are okfpub's write-back bookkeeping, not consumer-semantic
// columns. Against a data source holding only its title and a schema that declares
// none of the four, Provision creates all four as rich_text, so write-back to a
// fresh data source does not 400 on a missing hash/anchors column (#96).
func TestProvisionAddsSelfDescribingColumns(t *testing.T) {
	f := newFakeNotion()
	f.dsProps = map[string]map[string]any{
		"Name": {"type": "title"}, // a fresh data source ships with its title
	}
	// A schema declaring none of the self-describing columns.
	minimal := &schema.Schema{Columns: map[string]schema.Column{
		"Name":   {Kind: "title", Source: "derived"},
		"status": {Kind: "select", Source: "frontmatter", Options: []string{"proposed", "accepted"}},
	}}
	be := newServer(t, f, WithSchema(minimal))

	if err := be.Provision(context.Background()); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	patches := f.requestsTo("PATCH", "/data_sources/ds1")
	if len(patches) != 1 {
		t.Fatalf("want 1 PATCH /data_sources, got %d", len(patches))
	}
	added, _ := patches[0].Body["properties"].(map[string]any)
	if added == nil {
		t.Fatalf("PATCH body missing properties: %v", patches[0].Body)
	}

	for _, name := range []string{"path", "hash", "hashes", "anchors"} {
		def, ok := added[name].(map[string]any)
		if !ok {
			t.Errorf("self-describing column %q not provisioned, added = %v", name, added)
			continue
		}
		if _, ok := def["rich_text"]; !ok {
			t.Errorf("self-describing column %q should be provisioned as rich_text, got %v", name, def)
		}
	}
}

// TestProvisionSelfDescribingCaseInsensitive: a self-describing column that already
// exists under a different casing (e.g. "Anchors") is not re-added — the path
// queued()/EqualFold guards. Only the genuinely-missing self-describing columns are
// provisioned.
func TestProvisionSelfDescribingCaseInsensitive(t *testing.T) {
	f := newFakeNotion()
	f.dsProps = map[string]map[string]any{
		"Name":    {"type": "title"},
		"Anchors": {"type": "rich_text"}, // self-describing "anchors", differently cased
	}
	minimal := &schema.Schema{Columns: map[string]schema.Column{
		"Name": {Kind: "title", Source: "derived"},
	}}
	be := newServer(t, f, WithSchema(minimal))

	if err := be.Provision(context.Background()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	patches := f.requestsTo("PATCH", "/data_sources/ds1")
	if len(patches) != 1 {
		t.Fatalf("want 1 PATCH, got %d", len(patches))
	}
	added, _ := patches[0].Body["properties"].(map[string]any)
	if _, ok := added["anchors"]; ok {
		t.Errorf("anchors already exists (as Anchors) and must not be re-added, got %v", added["anchors"])
	}
	// The other three self-describing columns are still provisioned.
	for _, name := range []string{"path", "hash", "hashes"} {
		if _, ok := added[name].(map[string]any); !ok {
			t.Errorf("self-describing column %q should still be provisioned, added = %v", name, added)
		}
	}
}

// TestProvisionAddsMissingTypedColumns: against a data source holding only its
// title, Provision adds every other schema column with the Notion type its kind
// maps to, and carries a select's options — the core of #82(A).
func TestProvisionAddsMissingTypedColumns(t *testing.T) {
	f := newFakeNotion()
	f.dsProps = map[string]map[string]any{
		"Name": {"type": "title"}, // a fresh data source ships with its title
	}
	be := newServer(t, f, WithSchema(ideasSchema()))

	if err := be.Provision(context.Background()); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	patches := f.requestsTo("PATCH", "/data_sources/ds1")
	if len(patches) != 1 {
		t.Fatalf("want 1 PATCH /data_sources, got %d", len(patches))
	}
	added, _ := patches[0].Body["properties"].(map[string]any)
	if added == nil {
		t.Fatalf("PATCH body missing properties: %v", patches[0].Body)
	}

	// Title never provisioned (a data source already has exactly one).
	if _, ok := added["Name"]; ok {
		t.Errorf("title column must not be provisioned, got %v", added["Name"])
	}

	wantType := map[string]string{
		"path":        "rich_text",
		"type":        "select",
		"hashes":      "rich_text",
		"identifier":  "rich_text",
		"status":      "select",
		"domain":      "select",
		"tags":        "multi_select",
		"created":     "date",
		"updated":     "date",
		"description": "rich_text",
		"priority":    "number",
		"pinned":      "checkbox",
	}
	for name, typ := range wantType {
		def, ok := added[name].(map[string]any)
		if !ok {
			t.Errorf("column %q not provisioned, added = %v", name, added)
			continue
		}
		if _, ok := def[typ]; !ok {
			t.Errorf("column %q should be provisioned as %q, got %v", name, typ, def)
		}
	}

	// A select carries its options.
	sel, _ := added["status"].(map[string]any)["select"].(map[string]any)
	opts, _ := sel["options"].([]any)
	if len(opts) != 2 {
		t.Errorf("status select should carry its 2 options, got %v", sel["options"])
	}
}

// TestProvisionIdempotent: a fully-provisioned data source issues no PATCH — the
// near-noop property the scheduled path relies on.
func TestProvisionIdempotent(t *testing.T) {
	f := newFakeNotion()
	f.dsProps = map[string]map[string]any{
		"Name":        {"type": "title"},
		"path":        {"type": "rich_text"},
		"hash":        {"type": "rich_text"}, // okfpub self-describing
		"anchors":     {"type": "rich_text"}, // okfpub self-describing
		"type":        {"type": "select"},
		"hashes":      {"type": "rich_text"},
		"identifier":  {"type": "rich_text"},
		"status":      {"type": "select"},
		"domain":      {"type": "select"},
		"tags":        {"type": "multi_select"},
		"created":     {"type": "date"},
		"updated":     {"type": "date"},
		"description": {"type": "rich_text"},
		"priority":    {"type": "number"},
		"pinned":      {"type": "checkbox"},
	}
	be := newServer(t, f, WithSchema(ideasSchema()))

	if err := be.Provision(context.Background()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if n := f.countPath("PATCH", "/data_sources/ds1"); n != 0 {
		t.Errorf("a fully-provisioned data source should issue no PATCH, got %d", n)
	}
}

// TestProvisionCaseInsensitive: an existing column matches a schema column by a
// case-insensitive name, so a differently-cased title/column is not re-added.
func TestProvisionCaseInsensitive(t *testing.T) {
	f := newFakeNotion()
	f.dsProps = map[string]map[string]any{
		"Name":   {"type": "title"},
		"STATUS": {"type": "select"}, // schema declares "status"
	}
	be := newServer(t, f, WithSchema(ideasSchema()))

	if err := be.Provision(context.Background()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	patches := f.requestsTo("PATCH", "/data_sources/ds1")
	if len(patches) != 1 {
		t.Fatalf("want 1 PATCH, got %d", len(patches))
	}
	added, _ := patches[0].Body["properties"].(map[string]any)
	if _, ok := added["status"]; ok {
		t.Errorf("status already exists (as STATUS) and must not be re-added, got %v", added["status"])
	}
}

// TestProvisionNoSchemaNoOp: with no schema the backend provisions nothing — it
// does not even read the data source.
func TestProvisionNoSchemaNoOp(t *testing.T) {
	f := newFakeNotion()
	be := newServer(t, f) // no schema

	if err := be.Provision(context.Background()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if n := f.countPath("GET", "/data_sources/ds1"); n != 0 {
		t.Errorf("no schema should read no data source, got %d GETs", n)
	}
	if n := f.countPath("PATCH", "/data_sources/ds1"); n != 0 {
		t.Errorf("no schema should issue no PATCH, got %d", n)
	}
}
