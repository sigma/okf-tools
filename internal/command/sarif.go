package command

import (
	"encoding/json"
	"io"

	"github.com/sigma/okf-tools/internal/bundle"
	"github.com/sigma/okf-tools/internal/rules"
)

// SARIF 2.1.0 output for `okftool lint --format sarif`, so CI can upload lint
// findings to GitHub code scanning. Only the fields consumers need are modelled.

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	InformationURI string      `json:"informationUri"`
	Rules          []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	ShortDescription sarifText          `json:"shortDescription"`
	DefaultConfig    sarifDefaultConfig `json:"defaultConfiguration"`
}

type sarifDefaultConfig struct {
	Level string `json:"level"`
}

type sarifText struct {
	Text string `json:"text"`
}

type sarifResult struct {
	RuleID    string          `json:"ruleId"`
	Level     string          `json:"level"`
	Message   sarifText       `json:"message"`
	Locations []sarifLocation `json:"locations,omitempty"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysical `json:"physicalLocation"`
}

type sarifPhysical struct {
	ArtifactLocation sarifArtifact `json:"artifactLocation"`
	Region           *sarifRegion  `json:"region,omitempty"`
}

type sarifArtifact struct {
	URI string `json:"uri"`
}

type sarifRegion struct {
	StartLine int `json:"startLine"`
}

// sarifLevel maps an okf severity to a SARIF result level.
func sarifLevel(s rules.Severity) string {
	switch s {
	case rules.Error:
		return "error"
	case rules.Warning:
		return "warning"
	case rules.Info:
		return "note"
	default:
		return "none"
	}
}

func renderSARIF(w io.Writer, b *bundle.Bundle, findings []rules.Finding) error {
	driver := sarifDriver{
		Name:           "okftool",
		InformationURI: "https://github.com/sigma/okf-tools",
	}
	for _, r := range rules.All() {
		driver.Rules = append(driver.Rules, sarifRule{
			ID:               r.ID,
			Name:             r.Name,
			ShortDescription: sarifText{Text: r.Name},
			DefaultConfig:    sarifDefaultConfig{Level: sarifLevel(rules.Effective(r, b.Config))},
		})
	}

	results := make([]sarifResult, 0, len(findings))
	for _, f := range findings {
		loc := sarifLocation{PhysicalLocation: sarifPhysical{ArtifactLocation: sarifArtifact{URI: f.Path}}}
		if f.Line > 0 {
			loc.PhysicalLocation.Region = &sarifRegion{StartLine: f.Line}
		}
		results = append(results, sarifResult{
			RuleID:    f.Rule,
			Level:     sarifLevel(f.Severity),
			Message:   sarifText{Text: f.Message},
			Locations: []sarifLocation{loc},
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(sarifLog{
		Schema:  "https://json.schemastore.org/sarif-2.1.0.json",
		Version: "2.1.0",
		Runs:    []sarifRun{{Tool: sarifTool{Driver: driver}, Results: results}},
	})
}
