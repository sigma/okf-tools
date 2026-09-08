package pipeline

import (
	"context"
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

// TestLoadConfigReadsSurface loads the config surface this package owns:
// schema.json through the shared loader, plus the two credentials from the
// injected environment. areas.json is deliberately absent — the bundle parses it,
// and this package reads the result from there rather than parsing it again.
func TestLoadConfigReadsSurface(t *testing.T) {
	dir := t.TempDir()
	schemaPath := writeFile(t, dir, "schema.json", `{
		"Name":  {"kind": "title", "source": "frontmatter"},
		"hash":  {"kind": "text",  "source": "derived"}
	}`)

	env := map[string]string{"NOTION_TOKEN": "secret-tok", "NOTION_DB_ID": "ds-42"}
	cfg, err := LoadConfig(LoadOptions{
		SchemaPath: schemaPath,
		Getenv:     func(k string) string { return env[k] },
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.NotionToken != "secret-tok" || cfg.NotionDBID != "ds-42" {
		t.Errorf("credentials = %q/%q, want secret-tok/ds-42", cfg.NotionToken, cfg.NotionDBID)
	}
	if cfg.Schema == nil {
		t.Fatal("schema not loaded")
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

// TestLoadConfigOptionalFiles: an absent schema path is not an error (schema.json
// is optional in the contract).
func TestLoadConfigOptionalFiles(t *testing.T) {
	cfg, err := LoadConfig(LoadOptions{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("LoadConfig with no files: %v", err)
	}
	if cfg.Schema != nil {
		t.Errorf("no path given, want nil schema, got %v", cfg.Schema)
	}
}

// TestLoadConfigRejectsMalformed: a malformed schema.json fails the load loudly
// rather than publishing against a half-understood contract.
func TestLoadConfigRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	bad := writeFile(t, dir, "schema.json", `{"Name": {"kind":`) // truncated JSON
	if _, err := LoadConfig(LoadOptions{SchemaPath: bad, Getenv: func(string) string { return "" }}); err == nil {
		t.Fatal("want an error for a malformed schema.json")
	}
}

// TestSelectBackend: fake needs no creds; notion requires both and is refused
// without them; an unknown kind errors.
func TestSelectBackend(t *testing.T) {
	if _, err := SelectBackend(context.Background(), BackendFake, &Config{}, "bundle"); err != nil {
		t.Errorf("fake backend needs no credentials: %v", err)
	}

	// The fs/export backend needs no credentials either — it targets the filesystem.
	if _, err := SelectBackend(context.Background(), BackendFS, &Config{OutDir: t.TempDir()}, "bundle"); err != nil {
		t.Errorf("fs backend needs no credentials: %v", err)
	}
	if _, err := SelectBackend(context.Background(), BackendFS, &Config{}, "bundle"); err != nil {
		t.Errorf("fs backend should default its out dir when none is given: %v", err)
	}

	if _, err := SelectBackend(context.Background(), BackendNotion, &Config{NotionDBID: "ds"}, "bundle"); err == nil {
		t.Error("notion without a token should be refused")
	}
	if _, err := SelectBackend(context.Background(), BackendNotion, &Config{NotionToken: "tok"}, "bundle"); err == nil {
		t.Error("notion without a db id should be refused")
	}
	if _, err := SelectBackend(context.Background(), BackendNotion, &Config{NotionToken: "tok", NotionDBID: "ds"}, "bundle"); err != nil {
		t.Errorf("notion with both credentials should build: %v", err)
	}

	if _, err := SelectBackend(context.Background(), BackendKind("bogus"), &Config{}, "bundle"); err == nil {
		t.Error("unknown backend kind should error")
	}
}
