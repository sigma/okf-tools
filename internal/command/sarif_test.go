package command

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sigma/okf-tools/internal/rules"
)

func TestRenderSARIF(t *testing.T) {
	b := loadFixture(t, fixtureDir("okf106"))
	findings := rules.Run(&rules.Context{Bundle: b, Config: b.Config}, nil, nil)

	var buf bytes.Buffer
	if err := renderSARIF(&buf, b, findings); err != nil {
		t.Fatalf("renderSARIF: %v", err)
	}
	var log map[string]any
	if err := json.Unmarshal(buf.Bytes(), &log); err != nil {
		t.Fatalf("SARIF is not valid JSON: %v", err)
	}
	if log["version"] != "2.1.0" {
		t.Errorf("version = %v, want 2.1.0", log["version"])
	}
	for _, want := range []string{`"name": "okftool"`, `"ruleId": "OKF106"`, `"level": "warning"`, `"uri": "index.md"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("SARIF output missing %q", want)
		}
	}
}
