package pipeline

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFile materializes a config file in dir and returns its path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLoadConfigReadsSurface loads the whole config surface: areas.json and
// schema.json through the shared loaders, plus the two credentials from the
// injected environment.
func TestLoadConfigReadsSurface(t *testing.T) {
	dir := t.TempDir()
	areasPath := writeFile(t, dir, "areas.json", `{
		"docs":     {"directory": "docs", "type": "adr"},
		"glossary": {"file": "CONTEXT.md", "type": "glossary", "role": "glossary"}
	}`)
	schemaPath := writeFile(t, dir, "schema.json", `{
		"Name":  {"kind": "title", "source": "frontmatter"},
		"hash":  {"kind": "text",  "source": "derived"}
	}`)

	env := map[string]string{"NOTION_TOKEN": "secret-tok", "NOTION_DB_ID": "ds-42"}
	cfg, err := LoadConfig(LoadOptions{
		AreasPath:  areasPath,
		SchemaPath: schemaPath,
		Getenv:     func(k string) string { return env[k] },
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.NotionToken != "secret-tok" || cfg.NotionDBID != "ds-42" {
		t.Errorf("credentials = %q/%q, want secret-tok/ds-42", cfg.NotionToken, cfg.NotionDBID)
	}
	if cfg.Areas == nil || cfg.Schema == nil {
		t.Fatalf("areas/schema not loaded: areas=%v schema=%v", cfg.Areas, cfg.Schema)
	}
	if f, ok := cfg.GlossaryFile(); !ok || f != "CONTEXT.md" {
		t.Errorf("GlossaryFile() = %q,%v, want CONTEXT.md from the role marker", f, ok)
	}
	if _, ok := cfg.Schema.Lookup("Name"); !ok {
		t.Errorf("schema should declare the Name column")
	}
}

// TestLoadConfigArgsOverrideEnv: explicit args win over the environment, and the
// environment is the fallback.
func TestLoadConfigArgsOverrideEnv(t *testing.T) {
	env := map[string]string{"NOTION_TOKEN": "env-tok", "NOTION_DB_ID": "env-db"}
	getenv := func(k string) string { return env[k] }

	cfg, err := LoadConfig(LoadOptions{Token: "arg-tok", Getenv: getenv})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.NotionToken != "arg-tok" {
		t.Errorf("token = %q, want the arg to override env", cfg.NotionToken)
	}
	if cfg.NotionDBID != "env-db" {
		t.Errorf("db id = %q, want the env fallback", cfg.NotionDBID)
	}
}

// TestLoadConfigOptionalFiles: absent areas/schema paths are not an error (both are
// optional in the contract), and a nil registry resolves no glossary.
func TestLoadConfigOptionalFiles(t *testing.T) {
	cfg, err := LoadConfig(LoadOptions{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("LoadConfig with no files: %v", err)
	}
	if cfg.Areas != nil || cfg.Schema != nil {
		t.Errorf("no paths given, want nil areas/schema, got %v/%v", cfg.Areas, cfg.Schema)
	}
	if _, ok := cfg.GlossaryFile(); ok {
		t.Errorf("no registry, want no glossary file")
	}
}

// TestLoadConfigRejectsMalformed: a malformed areas.json fails the load loudly.
func TestLoadConfigRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	bad := writeFile(t, dir, "areas.json", `{"x": {"type": "t"}}`) // names neither dir nor file
	if _, err := LoadConfig(LoadOptions{AreasPath: bad, Getenv: func(string) string { return "" }}); err == nil {
		t.Fatal("want an error for a malformed areas.json")
	}
}

// TestSelectBackend: fake needs no creds; notion requires both and is refused
// without them; an unknown kind errors.
func TestSelectBackend(t *testing.T) {
	if _, err := SelectBackend(BackendFake, &Config{}); err != nil {
		t.Errorf("fake backend needs no credentials: %v", err)
	}

	if _, err := SelectBackend(BackendNotion, &Config{NotionDBID: "ds"}); err == nil {
		t.Error("notion without a token should be refused")
	}
	if _, err := SelectBackend(BackendNotion, &Config{NotionToken: "tok"}); err == nil {
		t.Error("notion without a db id should be refused")
	}
	if _, err := SelectBackend(BackendNotion, &Config{NotionToken: "tok", NotionDBID: "ds"}); err != nil {
		t.Errorf("notion with both credentials should build: %v", err)
	}

	if _, err := SelectBackend(BackendKind("bogus"), &Config{}); err == nil {
		t.Error("unknown backend kind should error")
	}
}
