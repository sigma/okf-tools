package notion

import (
	"testing"
	"time"

	"github.com/sigma/okf-tools/internal/schema"
)

// ideasSchema mirrors the shipping schema.json (ideas): a title, derived text
// columns, three selects, a list, two dates, and text frontmatter columns. It is
// the fixture both the typed-value and provisioning tests key off.
func ideasSchema() *schema.Schema {
	return &schema.Schema{Columns: map[string]schema.Column{
		"Name":        {Kind: "title", Source: "derived"},
		"path":        {Kind: "text", Source: "derived"},
		"type":        {Kind: "select", Source: "derived", Options: []string{"context", "adr", "idea"}},
		"hashes":      {Kind: "text", Source: "derived"},
		"identifier":  {Kind: "text", Source: "frontmatter"},
		"status":      {Kind: "select", Source: "frontmatter", Options: []string{"proposed", "accepted"}},
		"domain":      {Kind: "select", Source: "frontmatter", Options: []string{"product", "company"}},
		"tags":        {Kind: "list", Source: "frontmatter"},
		"created":     {Kind: "date", Source: "frontmatter"},
		"updated":     {Kind: "date", Source: "frontmatter"},
		"description": {Kind: "text", Source: "frontmatter"},
		"priority":    {Kind: "number", Source: "frontmatter"},
		"pinned":      {Kind: "checkbox", Source: "frontmatter"},
	}}
}

// TestPropsJSONTypedBySchemaKind proves each property is serialized in the shape
// its column's kind declares — the core of #82(B). One case per kind, plus the
// title (whose neutral key "title" is not a schema column name, so it exercises
// the legacy title fallback the schema path must preserve).
func TestPropsJSONTypedBySchemaKind(t *testing.T) {
	be := New(WithSchema(ideasSchema()))

	props := map[string]any{
		"title":       "Repo structure",       // title (via fallback: no "title" column)
		"type":        "adr",                  // select (derived)
		"status":      "accepted",             // select (frontmatter)
		"tags":        []any{"vcs", "layout"}, // list -> multi_select
		"created":     "2026-07-18",           // date (string form)
		"description": "Organize by intent",   // text -> rich_text
		"priority":    3,                      // number
		"pinned":      true,                   // checkbox
	}
	out := be.propsJSON(props)

	// title
	if _, ok := out["title"].(map[string]any)["title"]; !ok {
		t.Errorf("title should be a title value, got %v", out["title"])
	}
	// select
	sel, ok := out["status"].(map[string]any)["select"].(map[string]any)
	if !ok || sel["name"] != "accepted" {
		t.Errorf("status should be select{name:accepted}, got %v", out["status"])
	}
	if _, ok := out["type"].(map[string]any)["select"].(map[string]any); !ok {
		t.Errorf("derived type should be a select value, got %v", out["type"])
	}
	// multi_select
	ms, ok := out["tags"].(map[string]any)["multi_select"].([]map[string]any)
	if !ok || len(ms) != 2 || ms[0]["name"] != "vcs" || ms[1]["name"] != "layout" {
		t.Errorf("tags should be multi_select of [vcs layout], got %v", out["tags"])
	}
	// date
	d, ok := out["created"].(map[string]any)["date"].(map[string]any)
	if !ok || d["start"] != "2026-07-18" {
		t.Errorf("created should be date{start:2026-07-18}, got %v", out["created"])
	}
	// text
	if _, ok := out["description"].(map[string]any)["rich_text"]; !ok {
		t.Errorf("description should be a rich_text value, got %v", out["description"])
	}
	// number
	if n := out["priority"].(map[string]any)["number"]; n != 3 {
		t.Errorf("priority should be number 3, got %v", out["priority"])
	}
	// checkbox
	if c := out["pinned"].(map[string]any)["checkbox"]; c != true {
		t.Errorf("pinned should be checkbox true, got %v", out["pinned"])
	}
}

// TestPropsJSONNumberAndBoolCoercions covers the string-coercion branches of the
// number and checkbox builders — a numeric string parses, a non-numeric one clears
// the column, and only the conventional affirmatives read truthy.
func TestPropsJSONNumberAndBoolCoercions(t *testing.T) {
	be := New(WithSchema(ideasSchema()))

	if n := be.propsJSON(map[string]any{"priority": "42"})["priority"].(map[string]any)["number"]; n != 42.0 {
		t.Errorf(`number from "42" should parse to 42, got %v`, n)
	}
	if n := be.propsJSON(map[string]any{"priority": "high"})["priority"].(map[string]any)["number"]; n != nil {
		t.Errorf(`non-numeric number should clear the column (nil), got %v`, n)
	}
	if c := be.propsJSON(map[string]any{"pinned": "yes"})["pinned"].(map[string]any)["checkbox"]; c != true {
		t.Errorf(`checkbox from "yes" should be true, got %v`, c)
	}
	if c := be.propsJSON(map[string]any{"pinned": "nope"})["pinned"].(map[string]any)["checkbox"]; c != false {
		t.Errorf(`checkbox from "nope" should be false, got %v`, c)
	}
}

// TestPropsJSONSchemaPresentUnknownKeyFallsBack proves AC6's cited regression case:
// with a schema present, a key the schema does not declare (a write-back column
// like path/hash/anchors) still serializes as rich_text rather than erroring.
func TestPropsJSONSchemaPresentUnknownKeyFallsBack(t *testing.T) {
	be := New(WithSchema(ideasSchema()))
	out := be.propsJSON(map[string]any{"anchors": `{"x":"y"}`})
	if _, ok := out["anchors"].(map[string]any)["rich_text"]; !ok {
		t.Errorf("an undeclared key should fall back to rich_text, got %v", out["anchors"])
	}
}

// TestPropsJSONDateFromTime covers the yaml.v3 shape: an unquoted frontmatter date
// decodes to time.Time, which must normalize to a date-only ISO start.
func TestPropsJSONDateFromTime(t *testing.T) {
	be := New(WithSchema(ideasSchema()))
	out := be.propsJSON(map[string]any{
		"created": time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC),
	})
	d := out["created"].(map[string]any)["date"].(map[string]any)
	if d["start"] != "2026-07-18" {
		t.Errorf("time.Time date should format date-only, got %v", d["start"])
	}
}

// TestPropsJSONEmptySelectClears proves a present-but-empty select value clears the
// column (select:nil) rather than minting a blank option.
func TestPropsJSONEmptySelectClears(t *testing.T) {
	be := New(WithSchema(ideasSchema()))
	out := be.propsJSON(map[string]any{"status": ""})
	v := out["status"].(map[string]any)
	if got, ok := v["select"]; !ok || got != nil {
		t.Errorf("empty select should serialize as select:nil, got %v", v)
	}
}

// TestPropsJSONNoSchemaFallback proves the legacy split still holds with no schema:
// the literal "title" key is a title, every other key rich_text.
func TestPropsJSONNoSchemaFallback(t *testing.T) {
	be := New() // no schema

	out := be.propsJSON(map[string]any{"title": "T", "status": "accepted"})
	if _, ok := out["title"].(map[string]any)["title"]; !ok {
		t.Errorf("title should be a title value, got %v", out["title"])
	}
	// Without a schema, status has no declared kind and must stay rich_text.
	if _, ok := out["status"].(map[string]any)["rich_text"]; !ok {
		t.Errorf("without a schema, status should fall back to rich_text, got %v", out["status"])
	}
}
